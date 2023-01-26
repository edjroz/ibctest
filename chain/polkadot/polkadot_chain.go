package polkadot

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"

	"github.com/StirlingMarketingGroup/go-namecase"
	"github.com/docker/docker/api/types"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/icza/dyno"
	p2pcrypto "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/strangelove-ventures/ibctest/v6/ibc"
	"github.com/strangelove-ventures/ibctest/v6/internal/dockerutil"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// PolkadotChain implements the ibc.Chain interface for substrate chains.
type PolkadotChain struct {
	log                *zap.Logger
	testName           string
	cfg                ibc.ChainConfig
	numRelayChainNodes int
	parachainConfig    []ParachainConfig
	RelayChainNodes    RelayChainNodes
	ParachainNodes     []ParachainNodes
	publicKey          []byte
}

// PolkadotAuthority is used when constructing the validator authorities in the substrate chain spec.
type PolkadotAuthority struct {
	Grandpa            string `json:"grandpa"`
	Babe               string `json:"babe"`
	IMOnline           string `json:"im_online"`
	ParachainValidator string `json:"parachain_validator"`
	AuthorityDiscovery string `json:"authority_discovery"`
	ParaValidator      string `json:"para_validator"`
	ParaAssignment     string `json:"para_assignment"`
	Beefy              string `json:"beefy"`
}

// PolkadotParachainSpec is used when constructing substrate chain spec for parachains.
type PolkadotParachainSpec struct {
	GenesisHead    string `json:"genesis_head"`
	ValidationCode string `json:"validation_code"`
	Parachain      bool   `json:"parachain"`
}

// ParachainConfig is a shared type that allows callers of this module to configure a parachain.
type ParachainConfig struct {
	ChainID         string
	ChainName       string
	Bin             string
	Image           ibc.DockerImage
	NumNodes        int
	Flags           []string
	RelayChainFlags []string
	FinalityGadget  string
}

// IndexedName is a slice of the substrate dev key names used for key derivation.
var IndexedName = []string{"alice", "bob", "charlie", "dave", "ferdie"}

// NewPolkadotChain returns an uninitialized PolkadotChain, which implements the ibc.Chain interface.
func NewPolkadotChain(log *zap.Logger, testName string, chainConfig ibc.ChainConfig, numRelayChainNodes int, parachains []ParachainConfig) *PolkadotChain {
	return &PolkadotChain{
		log:                log,
		testName:           testName,
		cfg:                chainConfig,
		numRelayChainNodes: numRelayChainNodes,
		parachainConfig:    parachains,
	}
}

// Config fetches the chain configuration.
// Implements Chain interface.
func (c *PolkadotChain) Config() ibc.ChainConfig {
	return c.cfg
}

// Initialize initializes node structs so that things like initializing keys can be done before starting the chain.
// Implements Chain interface.
func (c *PolkadotChain) Initialize(ctx context.Context, testName string, cli *client.Client, networkID string) error {
	relayChainNodes := []*RelayChainNode{}
	chainCfg := c.Config()
	images := []ibc.DockerImage{}
	images = append(images, chainCfg.Images...)
	for _, parachain := range c.parachainConfig {
		images = append(images, parachain.Image)
	}
	for _, image := range images {
		rc, err := cli.ImagePull(
			ctx,
			image.Repository+":"+image.Version,
			types.ImagePullOptions{},
		)
		if err != nil {
			c.log.Error("Failed to pull image",
				zap.Error(err),
				zap.String("repository", image.Repository),
				zap.String("tag", image.Version),
			)
		} else {
			_, _ = io.Copy(io.Discard, rc)
			_ = rc.Close()
		}
	}
	for i := 0; i < c.numRelayChainNodes; i++ {
		seed := make([]byte, 32)
		rand.Read(seed)

		nodeKey, _, err := p2pcrypto.GenerateEd25519Key(crand.Reader)
		if err != nil {
			return fmt.Errorf("error generating node key: %w", err)
		}

		nameCased := namecase.New().NameCase(IndexedName[i])

		ed25519PrivKey, err := DeriveEd25519FromName(nameCased)
		if err != nil {
			return err
		}
		accountKey, err := DeriveSr25519FromName([]string{nameCased})
		if err != nil {
			return err
		}
		stashKey, err := DeriveSr25519FromName([]string{nameCased, "stash"})
		if err != nil {
			return err
		}
		ecdsaPrivKey, err := DeriveSecp256k1FromName(nameCased)
		if err != nil {
			return fmt.Errorf("error generating secp256k1 private key: %w", err)
		}
		pn := &RelayChainNode{
			log:               c.log,
			Index:             i,
			Chain:             c,
			DockerClient:      cli,
			NetworkID:         networkID,
			TestName:          testName,
			Image:             chainCfg.Images[0],
			NodeKey:           nodeKey,
			Ed25519PrivateKey: ed25519PrivKey,
			AccountKey:        accountKey,
			StashKey:          stashKey,
			EcdsaPrivateKey:   *ecdsaPrivKey,
		}

		v, err := cli.VolumeCreate(ctx, volumetypes.VolumeCreateBody{
			Labels: map[string]string{
				dockerutil.CleanupLabel: testName,

				dockerutil.NodeOwnerLabel: pn.Name(),
			},
		})
		if err != nil {
			return fmt.Errorf("creating volume for chain node: %w", err)
		}
		pn.VolumeName = v.Name

		if err := dockerutil.SetVolumeOwner(ctx, dockerutil.VolumeOwnerOptions{
			Log:        c.log,
			Client:     cli,
			VolumeName: v.Name,
			ImageRef:   chainCfg.Images[0].Ref(),
			TestName:   testName,
			UidGid:     chainCfg.Images[0].UidGid,
		}); err != nil {
			return fmt.Errorf("set volume owner: %w", err)
		}

		relayChainNodes = append(relayChainNodes, pn)
	}
	c.RelayChainNodes = relayChainNodes
	for _, parachainConfig := range c.parachainConfig {
		parachainNodes := []*ParachainNode{}
		for i := 0; i < parachainConfig.NumNodes; i++ {
			nodeKey, _, err := p2pcrypto.GenerateEd25519Key(crand.Reader)
			if err != nil {
				return fmt.Errorf("error generating node key: %w", err)
			}
			fmt.Println("Using ParachainNode.ChainName = ", parachainConfig.ChainName)
			fmt.Println("Using ParachainNode.ChainID = ", parachainConfig.ChainID)
			pn := &ParachainNode{
				log:             c.log,
				Index:           i,
				Chain:           c,
				DockerClient:    cli,
				NetworkID:       networkID,
				TestName:        testName,
				NodeKey:         nodeKey,
				Image:           parachainConfig.Image,
				Bin:             parachainConfig.Bin,
				ChainID:         parachainConfig.ChainID,
				Flags:           parachainConfig.Flags,
				RelayChainFlags: parachainConfig.RelayChainFlags,
				ChainName:       parachainConfig.ChainName,
			}
			v, err := cli.VolumeCreate(ctx, volumetypes.VolumeCreateBody{
				Labels: map[string]string{
					dockerutil.CleanupLabel:   testName,
					dockerutil.NodeOwnerLabel: pn.Name(),
				},
			})
			if err != nil {
				return fmt.Errorf("creating volume for chain node: %w", err)
			}
			pn.VolumeName = v.Name

			if err := dockerutil.SetVolumeOwner(ctx, dockerutil.VolumeOwnerOptions{
				Log:        c.log,
				Client:     cli,
				VolumeName: v.Name,
				ImageRef:   parachainConfig.Image.Ref(),
				TestName:   testName,
				UidGid:     parachainConfig.Image.UidGid,
			}); err != nil {
				return fmt.Errorf("set volume owner: %w", err)
			}
			parachainNodes = append(parachainNodes, pn)
		}
		c.ParachainNodes = append(c.ParachainNodes, parachainNodes)
	}

	return nil
}

func runtimeGenesisPath(path ...interface{}) []interface{} {
	fullPath := []interface{}{"genesis", "runtime", "runtime_genesis_config"}
	fullPath = append(fullPath, path...)
	return fullPath
}

func (c *PolkadotChain) modifyGenesis(ctx context.Context, chainSpec interface{}) error {
	bootNodes := []string{}
	authorities := [][]interface{}{}
	balances := [][]interface{}{}
	var sudoAddress string
	for i, n := range c.RelayChainNodes {
		multiAddress, err := n.MultiAddress()
		if err != nil {
			return err
		}
		bootNodes = append(bootNodes, multiAddress)
		stashAddress, err := n.StashAddress()
		if err != nil {
			return fmt.Errorf("error getting stash address")
		}
		accountAddress, err := n.AccountAddress()
		if err != nil {
			return fmt.Errorf("error getting account address")
		}
		grandpaAddress, err := n.GrandpaAddress()
		if err != nil {
			return fmt.Errorf("error getting grandpa address")
		}
		beefyAddress, err := n.EcdsaAddress()
		if err != nil {
			return fmt.Errorf("error getting beefy address")
		}
		balances = append(balances,
			[]interface{}{stashAddress, uint64(1000000000000000000)},
			[]interface{}{accountAddress, uint64(1000000000000000000)},
		)
		if i == 0 {
			sudoAddress = accountAddress
		}
		authority := []interface{}{stashAddress, stashAddress, PolkadotAuthority{
			Grandpa:            grandpaAddress,
			Babe:               accountAddress,
			IMOnline:           accountAddress,
			ParachainValidator: accountAddress,
			AuthorityDiscovery: accountAddress,
			ParaValidator:      accountAddress,
			ParaAssignment:     accountAddress,
			Beefy:              beefyAddress,
		}}
		authorities = append(authorities, authority)
	}

	if err := dyno.Set(chainSpec, bootNodes, "bootNodes"); err != nil {
		return fmt.Errorf("error setting boot nodes: %w", err)
	}
	if err := dyno.Set(chainSpec, authorities, runtimeGenesisPath("session", "keys")...); err != nil {
		return fmt.Errorf("error setting authorities: %w", err)
	}
	if err := dyno.Set(chainSpec, balances, runtimeGenesisPath("balances", "balances")...); err != nil {
		return fmt.Errorf("error setting balances: %w", err)
	}
	if err := dyno.Set(chainSpec, sudoAddress, runtimeGenesisPath("sudo", "key")...); err != nil {
		return fmt.Errorf("error setting sudo key: %w", err)
	}
	/*
		if err := dyno.Set(chainSpec, sudoAddress, runtimeGenesisPath("bridgeRococoGrandpa", "owner")...); err != nil {
			return fmt.Errorf("error setting bridgeRococoGrandpa owner: %w", err)
		}
		if err := dyno.Set(chainSpec, sudoAddress, runtimeGenesisPath("bridgeWococoGrandpa", "owner")...); err != nil {
			return fmt.Errorf("error setting bridgeWococoGrandpa owner: %w", err)
		}
		if err := dyno.Set(chainSpec, sudoAddress, runtimeGenesisPath("bridgeRococoMessages", "owner")...); err != nil {
			return fmt.Errorf("error setting bridgeRococoMessages owner: %w", err)
		}
		if err := dyno.Set(chainSpec, sudoAddress, runtimeGenesisPath("bridgeWococoMessages", "owner")...); err != nil {
			return fmt.Errorf("error setting bridgeWococoMessages owner: %w", err)
		}
	*/
	if err := dyno.Set(chainSpec, 2, runtimeGenesisPath("configuration", "config", "validation_upgrade_delay")...); err != nil {
		return fmt.Errorf("error setting validation upgrade delay: %w", err)
	}
	parachains := [][]interface{}{}

	for _, parachainNodes := range c.ParachainNodes {
		firstParachainNode := parachainNodes[0]
		parachainID, err := firstParachainNode.ParachainID(ctx)
		if err != nil {
			return fmt.Errorf("error getting parachain ID: %w", err)
		}
		genesisState, err := firstParachainNode.ExportGenesisState(ctx)
		if err != nil {
			return fmt.Errorf("error exporting genesis state: %w", err)
		}
		genesisWasm, err := firstParachainNode.ExportGenesisWasm(ctx)
		if err != nil {
			return fmt.Errorf("error exporting genesis wasm: %w", err)
		}

		composableParachain := []interface{}{parachainID, PolkadotParachainSpec{
			GenesisHead:    genesisState,
			ValidationCode: genesisWasm,
			Parachain:      true,
		}}
		parachains = append(parachains, composableParachain)
	}

	if err := dyno.Set(chainSpec, parachains, runtimeGenesisPath("paras", "paras")...); err != nil {
		return fmt.Errorf("error setting parachains: %w", err)
	}
	return nil
}

func (c *PolkadotChain) logger() *zap.Logger {
	return c.log.With(
		zap.String("chain_id", c.cfg.ChainID),
		zap.String("name", c.cfg.Name),
		zap.String("test", c.testName),
	)
}

// Start sets up everything needed (validators, gentx, fullnodes, peering, additional accounts) for chain to start from genesis.
// Implements Chain interface.
func (c *PolkadotChain) Start(testName string, ctx context.Context, additionalGenesisWallets ...ibc.WalletAmount) error {
	// generate chain spec
	firstNode := c.RelayChainNodes[0]
	if err := firstNode.GenerateChainSpec(ctx); err != nil {
		return fmt.Errorf("error generating chain spec: %w", err)
	}
	fr := dockerutil.NewFileRetriever(c.logger(), firstNode.DockerClient, c.testName)
	fw := dockerutil.NewFileWriter(c.logger(), firstNode.DockerClient, c.testName)

	chainSpecBytes, err := fr.SingleFileContent(ctx, firstNode.VolumeName, firstNode.ChainSpecFilePathContainer())
	if err != nil {
		return fmt.Errorf("error reading chain spec: %w", err)
	}

	var chainSpec interface{}
	if err := json.Unmarshal(chainSpecBytes, &chainSpec); err != nil {
		return fmt.Errorf("error unmarshaling chain spec: %w", err)
	}

	if err := c.modifyGenesis(ctx, chainSpec); err != nil {
		return fmt.Errorf("error modifying genesis: %w", err)
	}

	editedChainSpec, err := json.MarshalIndent(chainSpec, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling modified chain spec: %w", err)
	}

	if err := fw.WriteFile(ctx, firstNode.VolumeName, firstNode.ChainSpecFilePathContainer(), editedChainSpec); err != nil {
		return fmt.Errorf("error writing modified chain spec: %w", err)
	}

	c.logger().Info("Generating raw chain spec", zap.String("container", firstNode.Name()))

	if err := firstNode.GenerateChainSpecRaw(ctx); err != nil {
		return err
	}

	rawChainSpecBytes, err := fr.SingleFileContent(ctx, firstNode.VolumeName, firstNode.RawChainSpecFilePathRelative())
	if err != nil {
		return fmt.Errorf("error reading chain spec: %w", err)
	}

	var eg errgroup.Group
	for i, n := range c.RelayChainNodes {
		n := n
		i := i
		eg.Go(func() error {
			if i != 0 {
				c.logger().Info("Copying raw chain spec", zap.String("container", n.Name()))
				if err := fw.WriteFile(ctx, n.VolumeName, n.RawChainSpecFilePathRelative(), rawChainSpecBytes); err != nil {
					return fmt.Errorf("error writing raw chain spec: %w", err)
				}
			}
			c.logger().Info("Creating container", zap.String("name", n.Name()))
			if err := n.CreateNodeContainer(ctx); err != nil {
				return err
			}
			c.logger().Info("Starting container", zap.String("name", n.Name()))
			return n.StartContainer(ctx)
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	c.logger().Info("Para nodes1: ", zap.String("nodes", fmt.Sprintf("%v", c.ParachainNodes)))
	c.logger().Info("Para nodes2: ", zap.Int("count", len(c.ParachainNodes)))
	for _, nodes := range c.ParachainNodes {
		nodes := nodes
		for _, n := range nodes {
			n := n
			eg.Go(func() error {
				c.logger().Info("Copying raw chain spec", zap.String("container", n.Name()))
				if err := fw.WriteFile(ctx, n.VolumeName, n.RawChainSpecFilePathRelative(), rawChainSpecBytes); err != nil {
					return fmt.Errorf("error writing raw chain spec: %w", err)
				}
				c.logger().Info("Creating container", zap.String("name", n.Name()))
				if err := n.CreateNodeContainer(ctx); err != nil {
					return err
				}
				c.logger().Info("Starting container", zap.String("name", n.Name()))
				return n.StartContainer(ctx)
			})
		}
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	return nil
}

// Exec runs an arbitrary command using Chain's docker environment.
// Implements Chain interface.
func (c *PolkadotChain) Exec(ctx context.Context, cmd []string, env []string) ([]byte, []byte, error) {
	res := c.RelayChainNodes[0].Exec(ctx, cmd, env)
	return res.Stdout, res.Stderr, res.Err
}

// GetRPCAddress retrieves the rpc address that can be reached by other containers in the docker network.
// Implements Chain interface.
func (c *PolkadotChain) GetRPCAddress() string {
	// time.Sleep(15 * time.Minute)
	var parachainHostName string
	port := strings.Split(rpcPort, "/")[0]

	if len(c.ParachainNodes) > 0 && len(c.ParachainNodes[0]) > 0 {
		parachainHostName = c.ParachainNodes[0][0].HostName()
	} else {
		parachainHostName = c.RelayChainNodes[0].HostName()
	}
	fmt.Println("GetRPCAddress: ", parachainHostName, rpcPort)

	relaychainHostName := c.RelayChainNodes[0].HostName()

	parachainUrl := fmt.Sprintf("http://%s:%s", parachainHostName, port)
	relaychainUrl := fmt.Sprintf("http://%s:%s", relaychainHostName, port)
	//return parachainUrl
	return fmt.Sprintf("%s,%s", parachainUrl, relaychainUrl)
}

//func (c *PolkadotChain) GetRPCAddress() string {
//	if len(c.ParachainNodes) > 0 && len(c.ParachainNodes[0]) > 0 {
//		return fmt.Sprintf("%s:%s", c.ParachainNodes[0][0].HostName(), strings.Split(rpcPort, "/")[0])
//	}
//	return fmt.Sprintf("%s:%s", c.RelayChainNodes[0].HostName(), strings.Split(rpcPort, "/")[0])
//}

// GetGRPCAddress retrieves the grpc address that can be reached by other containers in the docker network.
// Implements Chain interface.
func (c *PolkadotChain) GetGRPCAddress() string {
	if len(c.ParachainNodes) > 0 && len(c.ParachainNodes[0]) > 0 {
		return fmt.Sprintf("%s:%s", c.ParachainNodes[0][0].HostName(), strings.Split(wsPort, "/")[0])
	}
	return fmt.Sprintf("%s:%s", c.RelayChainNodes[0].HostName(), strings.Split(wsPort, "/")[0])
}

// GetHostRPCAddress returns the rpc address that can be reached by processes on the host machine.
// Note that this will not return a valid value until after Start returns.
// Implements Chain interface.
func (c *PolkadotChain) GetHostRPCAddress() string {
	if len(c.ParachainNodes) > 0 && len(c.ParachainNodes[0]) > 0 {
		return c.ParachainNodes[0][0].hostRpcPort
	}
	return c.RelayChainNodes[0].hostRpcPort
}

// GetHostGRPCAddress returns the grpc address that can be reached by processes on the host machine.
// Note that this will not return a valid value until after Start returns.
// Implements Chain interface.
func (c *PolkadotChain) GetHostGRPCAddress() string {
	if len(c.ParachainNodes) > 0 && len(c.ParachainNodes[0]) > 0 {
		return c.ParachainNodes[0][0].hostWsPort
	}
	return c.RelayChainNodes[0].hostWsPort
}

// Height returns the current block height or an error if unable to get current height.
// Implements Chain interface.
func (c *PolkadotChain) Height(ctx context.Context) (uint64, error) {
	if len(c.ParachainNodes) > 0 && len(c.ParachainNodes[0]) > 0 {
		block, err := c.ParachainNodes[0][0].api.RPC.Chain.GetBlockLatest()
		if err != nil {
			return 0, err
		}
		return uint64(block.Block.Header.Number), nil
	}
	block, err := c.RelayChainNodes[0].api.RPC.Chain.GetBlockLatest()
	if err != nil {
		return 0, err
	}
	return uint64(block.Block.Header.Number), nil
}

// ExportState exports the chain state at specific height.
// Implements Chain interface.
func (c *PolkadotChain) ExportState(ctx context.Context, height int64) (string, error) {
	panic("[ExportState] not implemented yet")
}

// HomeDir is the home directory of a node running in a docker container. Therefore, this maps to
// the container's filesystem (not the host).
// Implements Chain interface.
func (c *PolkadotChain) HomeDir() string {
	panic("[HomeDir] not implemented yet")
}

// CreateKey creates a test key in the "user" node (either the first fullnode or the first validator if no fullnodes).
// Implements Chain interface.
func (c *PolkadotChain) CreateKey(ctx context.Context, keyName string) error {
	// Alice's key
	publicKey, err := hex.DecodeString("020a1091341fe5664bfa1782d5e04779689068c916b04cb365ec3153755684d9a1")
	c.publicKey = publicKey
	return err
}

// RecoverKey recovers an existing user from a given mnemonic.
// Implements Chain interface.
func (c *PolkadotChain) RecoverKey(ctx context.Context, name, mnemonic string) error {
	panic("[RecoverKey] not implemented yet")
}

// GetAddress fetches the bech32 address for a test key on the "user" node (either the first fullnode or the first validator if no fullnodes).
// Implements Chain interface.
func (c *PolkadotChain) GetAddress(ctx context.Context, keyName string) ([]byte, error) {
	return c.publicKey, nil
}

// SendFunds sends funds to a wallet from a user account.
// Implements Chain interface.
func (c *PolkadotChain) SendFunds(ctx context.Context, keyName string, amount ibc.WalletAmount) error {
	fmt.Println("[PolkadotChain] SendFunds: ", keyName, amount)
	/*
		api := c.ParachainNodes[0][0].api
		meta, err := api.RPC.State.GetMetadataLatest()
		if err != nil {
			panic(err)
		}

		bob, err := types2.NewMultiAddressFromHexAccountID("0x8eaf04151687736326c9fea17e25fc5287613693c912909cb226aa4794f26a48")
		if err != nil {
			panic(err)
		}

		// 1 unit of transfer
		bal := new(big.Int).SetUint64(uint64(amount.Amount))

		call, err := types2.NewCall(meta, "Balances.transfer", bob, types2.NewUCompact(bal))
		if err != nil {
			panic(err)
		}

		// Create the extrinsic
		ext := types2.NewExtrinsic(call)

		genesisHash, err := api.RPC.Chain.GetBlockHash(0)
		if err != nil {
			panic(err)
		}

		rv, err := api.RPC.State.GetRuntimeVersionLatest()
		if err != nil {
			panic(err)
		}

		key, err := types2.CreateStorageKey(meta, "System", "Account", signature.TestKeyringPairAlice.PublicKey)
		if err != nil {
			panic(err)
		}

		var accountInfo types2.AccountInfo
		ok, err := api.RPC.State.GetStorageLatest(key, &accountInfo)
		if err != nil || !ok {
			panic(err)
		}

		nonce := uint32(accountInfo.Nonce)
		o := types2.SignatureOptions{
			BlockHash:          genesisHash,
			Era:                types2.ExtrinsicEra{IsMortalEra: false},
			GenesisHash:        genesisHash,
			Nonce:              types2.NewUCompactFromUInt(uint64(nonce)),
			SpecVersion:        rv.SpecVersion,
			Tip:                types2.NewUCompactFromUInt(100),
			TransactionVersion: rv.TransactionVersion,
		}

		// Sign the transaction using Alice's default account
		err = ext.Sign(signature.TestKeyringPairAlice, o)
		if err != nil {
			panic(err)
		}

		// Send the extrinsic
		_, err = api.RPC.Author.SubmitExtrinsic(ext)
		if err != nil {
			panic(err)
		}

		fmt.Printf("Balance transferred from Alice to Bob: %v\n", bal.String())
		//node.api.RPC.Author.SubmitAndWatchExtrinsic
		//c.ParachainNodes[0][0].api.RPC.
		//panic("[SendFunds] not implemented yet")

	*/
	return nil
}

// SendIBCTransfer sends an IBC transfer returning a transaction or an error if the transfer failed.
// Implements Chain interface.
func (c *PolkadotChain) SendIBCTransfer(ctx context.Context, channelID, keyName string, amount ibc.WalletAmount, timeout *ibc.IBCTimeout) (ibc.Tx, error) {
	fmt.Println("[PolkadotChain] SendIBCTransfer: ", channelID, keyName, amount, timeout)
	return ibc.Tx{
		Height:   0,
		TxHash:   "",
		GasSpent: 0,
		Packet:   ibc.Packet{},
	}, nil
}

// GetBalance fetches the current balance for a specific account address and denom.
// Implements Chain interface.
func (c *PolkadotChain) GetBalance(ctx context.Context, address string, denom string) (int64, error) {
	fmt.Println("[PolkadotChain] GetBalance: ", address, denom)
	return 0, nil
}

// GetGasFeesInNativeDenom gets the fees in native denom for an amount of spent gas.
// Implements Chain interface.
func (c *PolkadotChain) GetGasFeesInNativeDenom(gasPaid int64) int64 {
	panic("[GetGasFeesInNativeDenom] not implemented yet")
}

// Acknowledgements returns all acknowledgements in a block at height.
// Implements Chain interface.
func (c *PolkadotChain) Acknowledgements(ctx context.Context, height uint64) ([]ibc.PacketAcknowledgement, error) {
	fmt.Println("[PolkadotChain] Acknowledgements: ", height)
	return nil, nil
}

// Timeouts returns all timeouts in a block at height.
// Implements Chain interface.
func (c *PolkadotChain) Timeouts(ctx context.Context, height uint64) ([]ibc.PacketTimeout, error) {
	fmt.Println("[PolkadotChain] Timeouts: ", height)
	return nil, nil
}
