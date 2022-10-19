package polkadot_test

import (
	"context"
	"testing"
	"os"

	"github.com/strangelove-ventures/ibctest/v6"
	"github.com/strangelove-ventures/ibctest/v6/ibc"
	"github.com/strangelove-ventures/ibctest/v6/test"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	lightclient "github.com/ComposableFi/go-substrate-rpc-client/v4"
	"github.com/strangelove-ventures/ibctest/v6/testreporter"
	// rpcclienttypes "github.com/ComposableFi/go-substrate-rpc-client/v4/types"
	// beefytypes "github.com/ComposableFi/ics11-beefy/types"
)

var (
	BEEFY_TEST_MODE    = os.Getenv("BEEFY_TEST_MODE")
	RPC_CLIENT_ADDRESS = os.Getenv("RPC_CLIENT_ADDRESS")
	UPDATE_STATE_MODE  = os.Getenv("UPDATE_STATE_MODE")
)

func TestPolkadotComposableChainStart(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	t.Parallel()

	client, network := ibctest.DockerSetup(t)

	nv := 5
	nf := 3

	chains, err := ibctest.NewBuiltinChainFactory(zaptest.NewLogger(t), []*ibctest.ChainSpec{
		{
			Name:    "composable",
			Version: "polkadot:v0.9.19,composable:centauri",
			ChainConfig: ibc.ChainConfig{
				ChainID: "rococo-local",
			},
			NumValidators: &nv,
			NumFullNodes:  &nf,
		},
		{Name: "gaia", Version: "v7.0.0", ChainConfig: ibc.ChainConfig{
			GasPrices: "0.0uatom",
		}},
	},
	).Chains(t.Name())

	require.NoError(t, err, "failed to get polkadot chain")
	require.Len(t, chains, 2)
	chain := chains[0]
	chain2 := chains[1]

	ctx := context.Background()

	err = chain.Initialize(ctx, t.Name(), client, network)
	require.NoError(t, err, "failed to initialize polkadot chain")

	err = chain.Start(t.Name(), ctx)
	require.NoError(t, err, "failed to start polkadot chain")

	err = test.WaitForBlocks(ctx, 10, chain)
	require.NoError(t, err, "polkadot chain failed to make blocks")

	// beefy
	t.Skip("skipping TestCheckHeaderAndUpdateState")
	// if BEEFY_TEST_MODE != "true" {
	// 	t.Skip("skipping test in short mode")
	// }
	if RPC_CLIENT_ADDRESS == "" {
		t.Log("==== RPC_CLIENT_ADDRESS not set, will use default ==== ")
		RPC_CLIENT_ADDRESS = "ws://127.0.0.1:9944"
	}

	relayApi, err := lightclient.NewSubstrateAPI(RPC_CLIENT_ADDRESS)
	require.NoError(t, err)

	t.Log("==== connected! ==== ", relayApi)

	rep := testreporter.NewNopReporter()
	dClient, network := ibctest.DockerSetup(t)

	// Get a relayer instance
	r := ibctest.NewBuiltinRelayerFactory(
		ibc.CosmosRly,
		zaptest.NewLogger(t),
		// relayApi.RelayerOptionExtraStartFlags{Flags: []string{"-p", "events", "-b", "100"}},
	).Build(t, dClient, network)

	// Build the network; spin up the chains and configure the relayer
	const pathName = "test-path"
	const relayerName = "relayer"
	eRep := rep.RelayerExecReporter(t)


	ic := ibctest.NewInterchain().
		AddChain(chain).
		AddChain(chain2).
		AddRelayer(r, relayerName).
		AddLink(ibctest.InterchainLink{
			Chain1:  chain,
			Chain2:  chain2,
			Relayer: r,
			Path:    pathName,
		})

	require.NoError(t, ic.Build(ctx, eRep, ibctest.InterchainBuildOptions{
		TestName:         t.Name(),
		Client:           client,
		NetworkID:        network,
		SkipPathCreation: true,
	}))
}
