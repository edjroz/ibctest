package polkadot_test

import (
	"context"
	"testing"

	"github.com/strangelove-ventures/ibctest/v6"
	"github.com/strangelove-ventures/ibctest/v6/ibc"
	"github.com/strangelove-ventures/ibctest/v6/relayer"
	"github.com/strangelove-ventures/ibctest/v6/test"
	"github.com/strangelove-ventures/ibctest/v6/testreporter"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
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
	},
	).Chains(t.Name())

	require.NoError(t, err, "failed to get polkadot chain")
	require.Len(t, chains, 1)
	chain := chains[0]

	ctx := context.Background()

	err = chain.Initialize(ctx, t.Name(), client, network)
	require.NoError(t, err, "failed to initialize polkadot chain")

	err = chain.Start(t.Name(), ctx)
	require.NoError(t, err, "failed to start polkadot chain")

	err = test.WaitForBlocks(ctx, 10, chain)
	require.NoError(t, err, "polkadot chain failed to make blocks")
}

func TestCreatePolakdotCosmosLink(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	t.Parallel()

	rep := testreporter.NewNopReporter()
	eRep := rep.RelayerExecReporter(t)

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
		{
			Name:    "gaia",
			Version: "beefy",
			ChainConfig: ibc.ChainConfig{
				ChainID: "gaia",
				Images: []ibc.DockerImage{
					{
						Repository: "ghcr.io/oshorefueled/gaia",
						Version:    "beefy",
						UidGid:     "100:1000",
					},
				},
			},
		},
	},
	).Chains(t.Name())

	require.NoError(t, err, "failed to get polkadot chain")
	require.Len(t, chains, 2)
	chain1 := chains[0]
	chain2 := chains[1]

	ctx := context.Background()

	err = chain1.Initialize(ctx, t.Name(), client, network)
	require.NoError(t, err, "failed to initialize polkadot chain")

	err = chain1.Start(t.Name(), ctx)
	require.NoError(t, err, "failed to start polkadot chain")

	err = test.WaitForBlocks(ctx, 10, chain1)
	require.NoError(t, err, "polkadot chain failed to make blocks")

	err = chain2.Initialize(ctx, t.Name(), client, network)
	require.NoError(t, err, "failed to initialize gaia chain")

	err = chain2.Start(t.Name(), ctx)
	require.NoError(t, err, "failed to start gaia chain")

	err = test.WaitForBlocks(ctx, 10, chain2)
	require.NoError(t, err, "gaia chain failed to make blocks")

	r := ibctest.NewBuiltinRelayerFactory(ibc.CosmosRly, zaptest.NewLogger(t), relayer.ImagePull(false),
		relayer.CustomDockerImage("relayer", "create-client", "100:1000")).Build(
		t, client, network,
	)

	ic := ibctest.NewInterchain().
		AddChain(chain1).
		AddChain(chain2).
		AddRelayer(r, "r").
		AddLink(ibctest.InterchainLink{
			Chain1:  chain1,
			Chain2:  chain2,
			Relayer: r,
		})

	require.NoError(t, ic.Build(ctx, eRep, ibctest.InterchainBuildOptions{
		TestName:         t.Name(),
		Client:           client,
		NetworkID:        network,
		SkipPathCreation: false,
	}))
}
