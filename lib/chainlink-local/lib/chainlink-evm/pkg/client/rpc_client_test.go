package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
	"github.com/smartcontractkit/chainlink-framework/multinode"

	"github.com/smartcontractkit/chainlink-evm/pkg/client"
	"github.com/smartcontractkit/chainlink-evm/pkg/config/chaintype"
	"github.com/smartcontractkit/chainlink-evm/pkg/testutils"
	evmtypes "github.com/smartcontractkit/chainlink-evm/pkg/types"
)

func makeNewWSMessage[T any](v T) string {
	asJSON, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Errorf("failed to marshal head: %w", err))
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0x00","result":%s}}`, string(asJSON))
}

var makeNewHeadWSMessage = makeNewWSMessage[*evmtypes.Head]

func TestRPCClient_SubscribeToHeads(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(tests.Context(t), tests.WaitTimeout(t))
	defer cancel()

	chainId := big.NewInt(123456)
	lggr := logger.Test(t)

	nodePoolCfgHeadPolling := client.TestNodePoolConfig{
		NodeNewHeadsPollInterval:       1 * time.Second,
		NodeFinalizedBlockPollInterval: 1 * time.Second,
	}

	nodePoolCfgWSSub := client.TestNodePoolConfig{
		NodeFinalizedBlockPollInterval: 1 * time.Second,
	}

	serverCallBack := func(method string, params gjson.Result) (resp testutils.JSONRPCResponse) {
		if method == "eth_unsubscribe" {
			resp.Result = "true"
			return
		} else if method == "eth_subscribe" {
			assert.Equal(t, "eth_subscribe", method)
			if assert.True(t, params.IsArray()) && assert.Equal(t, "newHeads", params.Array()[0].String()) {
				resp.Result = `"0x00"`
			}
			return
		}
		return
	}

	checkClosedRPCClientShouldRemoveExistingSub := func(t tests.TestingT, ctx context.Context, sub multinode.Subscription, rpcClient *client.RPCClient) {
		errCh := sub.Err()

		rpcClient.UnsubscribeAllExcept()

		// ensure sub is closed
		select {
		case <-errCh: // ok
		default:
			assert.Fail(t, "channel should be closed")
		}

		require.NoError(t, rpcClient.Dial(ctx))
	}

	t.Run("WS and HTTP URL cannot be both empty", func(t *testing.T) {
		// ws is optional when LogBroadcaster is disabled, however SubscribeFilterLogs will return error if ws is missing
		observedLggr := logger.Test(t)
		rpcClient := client.NewRPCClient(nodePoolCfgHeadPolling, observedLggr, nil, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		require.Equal(t, errors.New("cannot dial rpc client when both ws and http info are missing"), rpcClient.Dial(ctx))
	})

	t.Run("Updates chain info on new blocks", func(t *testing.T) {
		server := testutils.NewWSServer(t, chainId, serverCallBack)
		wsURL := server.WSURL()

		rpc := client.NewRPCClient(nodePoolCfgWSSub, lggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		defer rpc.Close()
		require.NoError(t, rpc.Dial(ctx))
		// set to default values
		latest, highestUserObservations := rpc.GetInterceptedChainInfo()
		assert.Equal(t, int64(0), latest.BlockNumber)
		assert.Equal(t, int64(0), latest.FinalizedBlockNumber)
		assert.Nil(t, latest.TotalDifficulty)
		assert.Equal(t, int64(0), highestUserObservations.BlockNumber)
		assert.Equal(t, int64(0), highestUserObservations.FinalizedBlockNumber)
		assert.Nil(t, highestUserObservations.TotalDifficulty)

		ch, sub, err := rpc.SubscribeToHeads(tests.Context(t))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		go server.MustWriteBinaryMessageSync(t, makeNewHeadWSMessage(&evmtypes.Head{Number: 256, TotalDifficulty: big.NewInt(1000)}))
		// received 256 head
		<-ch
		go server.MustWriteBinaryMessageSync(t, makeNewHeadWSMessage(&evmtypes.Head{Number: 128, TotalDifficulty: big.NewInt(500)}))
		// received 128 head
		<-ch

		latest, highestUserObservations = rpc.GetInterceptedChainInfo()
		assert.Equal(t, int64(128), latest.BlockNumber)
		assert.Equal(t, int64(0), latest.FinalizedBlockNumber)
		assert.Equal(t, big.NewInt(500), latest.TotalDifficulty)

		assertHighestUserObservations := func(highestUserObservations multinode.ChainInfo) {
			assert.Equal(t, int64(256), highestUserObservations.BlockNumber)
			assert.Equal(t, int64(0), highestUserObservations.FinalizedBlockNumber)
			assert.Equal(t, big.NewInt(1000), highestUserObservations.TotalDifficulty)
		}

		assertHighestUserObservations(highestUserObservations)

		// Close resets latest
		rpc.Close()

		latest, highestUserObservations = rpc.GetInterceptedChainInfo()
		assert.Equal(t, int64(0), latest.BlockNumber)
		assert.Equal(t, int64(0), latest.FinalizedBlockNumber)
		assert.Nil(t, latest.TotalDifficulty)

		assertHighestUserObservations(highestUserObservations)
	})
	t.Run("App layer observations are not affected by new block if health check flag is present", func(t *testing.T) {
		server := testutils.NewWSServer(t, chainId, serverCallBack)
		wsURL := server.WSURL()

		rpc := client.NewRPCClient(nodePoolCfgWSSub, lggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		defer rpc.Close()
		require.NoError(t, rpc.Dial(ctx))

		ch, sub, err := rpc.SubscribeToHeads(multinode.CtxAddHealthCheckFlag(tests.Context(t)))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		go server.MustWriteBinaryMessageSync(t, makeNewHeadWSMessage(&evmtypes.Head{Number: 256, TotalDifficulty: big.NewInt(1000)}))
		// received 256 head
		<-ch

		latest, highestUserObservations := rpc.GetInterceptedChainInfo()
		assert.Equal(t, int64(256), latest.BlockNumber)
		assert.Equal(t, int64(0), latest.FinalizedBlockNumber)
		assert.Equal(t, big.NewInt(1000), latest.TotalDifficulty)

		assert.Equal(t, int64(0), highestUserObservations.BlockNumber)
		assert.Equal(t, int64(0), highestUserObservations.FinalizedBlockNumber)
		assert.Nil(t, highestUserObservations.TotalDifficulty)
	})
	t.Run("SubscribeToHeads with http polling enabled will update new heads", func(t *testing.T) {
		type rpcServer struct {
			Head *evmtypes.Head
			URL  *url.URL
		}
		createRPCServer := func() *rpcServer {
			server := &rpcServer{}
			server.URL = testutils.NewWSServer(t, chainId, func(method string, params gjson.Result) (resp testutils.JSONRPCResponse) {
				assert.Equal(t, "eth_getBlockByNumber", method)
				if assert.True(t, params.IsArray()) && assert.Equal(t, "latest", params.Array()[0].String()) {
					head := server.Head
					jsonHead, err := json.Marshal(head)
					if assert.NoError(t, err, "failed to marshal head") {
						resp.Result = string(jsonHead)
					}
				}

				return
			}).WSURL()
			return server
		}

		server := createRPCServer()
		rpc := client.NewRPCClient(nodePoolCfgHeadPolling, lggr, server.URL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		defer rpc.Close()
		require.NoError(t, rpc.Dial(ctx))
		latest, highestUserObservations := rpc.GetInterceptedChainInfo()
		// latest chain info hasn't been initialized
		assert.Equal(t, int64(0), latest.BlockNumber)
		assert.Equal(t, int64(0), highestUserObservations.BlockNumber)

		server.Head = &evmtypes.Head{Number: 127}
		headCh, sub, err := rpc.SubscribeToHeads(tests.Context(t))
		require.NoError(t, err)
		defer sub.Unsubscribe()

		head := <-headCh
		assert.Equal(t, server.Head.Number, head.BlockNumber())
		// should update both latest and user observations
		latest, highestUserObservations = rpc.GetInterceptedChainInfo()
		assert.Equal(t, int64(127), latest.BlockNumber)
		assert.Equal(t, int64(127), highestUserObservations.BlockNumber)

		// subscription with health check flag won't affect user observations
		sub.Unsubscribe() // stop prev subscription
		server.Head = &evmtypes.Head{Number: 256}
		headCh, sub, err = rpc.SubscribeToHeads(multinode.CtxAddHealthCheckFlag(tests.Context(t)))
		require.NoError(t, err)
		defer sub.Unsubscribe()

		head = <-headCh
		assert.Equal(t, server.Head.Number, head.BlockNumber())
		// should only update latest
		latest, highestUserObservations = rpc.GetInterceptedChainInfo()
		assert.Equal(t, int64(256), latest.BlockNumber)
		assert.Equal(t, int64(127), highestUserObservations.BlockNumber)
	})
	t.Run("Concurrent Unsubscribe and onNewHead calls do not lead to a deadlock", func(t *testing.T) {
		const numberOfAttempts = 1000 // need a large number to increase the odds of reproducing the issue
		server := testutils.NewWSServer(t, chainId, serverCallBack)
		wsURL := server.WSURL()

		rpc := client.NewRPCClient(nodePoolCfgWSSub, lggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		defer rpc.Close()
		require.NoError(t, rpc.Dial(ctx))
		var wg sync.WaitGroup
		for i := 0; i < numberOfAttempts; i++ {
			_, sub, err := rpc.SubscribeToHeads(tests.Context(t))
			require.NoError(t, err)
			wg.Add(2)
			go func() {
				server.MustWriteBinaryMessageSync(t, makeNewHeadWSMessage(&evmtypes.Head{Number: 256, TotalDifficulty: big.NewInt(1000)}))
				wg.Done()
			}()
			go func() {
				rpc.UnsubscribeAllExcept()
				sub.Unsubscribe()
				wg.Done()
			}()
			wg.Wait()
		}
	})
	t.Run("Block's chain ID matched configured", func(t *testing.T) {
		server := testutils.NewWSServer(t, chainId, serverCallBack)
		wsURL := server.WSURL()
		rpc := client.NewRPCClient(nodePoolCfgWSSub, lggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		defer rpc.Close()
		require.NoError(t, rpc.Dial(ctx))
		ch, sub, err := rpc.SubscribeToHeads(tests.Context(t))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		go server.MustWriteBinaryMessageSync(t, makeNewHeadWSMessage(&evmtypes.Head{Number: 256}))
		head := <-ch
		require.Equal(t, chainId, head.ChainID())
	})
	t.Run("Failed SubscribeToHeads returns and logs proper error", func(t *testing.T) {
		server := testutils.NewWSServer(t, chainId, func(reqMethod string, reqParams gjson.Result) (resp testutils.JSONRPCResponse) {
			return resp
		})
		wsURL := server.WSURL()
		observedLggr, observed := logger.TestObserved(t, zap.DebugLevel)
		rpc := client.NewRPCClient(nodePoolCfgWSSub, observedLggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		require.NoError(t, rpc.Dial(ctx))
		server.Close()
		_, _, err := rpc.SubscribeToHeads(ctx)
		require.ErrorContains(t, err, "RPCClient returned error (rpc)")
		tests.AssertLogEventually(t, observed, "evmclient.Client#EthSubscribe RPC call failure")
	})
	t.Run("Closed rpc client should remove existing SubscribeToHeads subscription with WS", func(t *testing.T) {
		server := testutils.NewWSServer(t, chainId, serverCallBack)
		wsURL := server.WSURL()
		rpc := client.NewRPCClient(nodePoolCfgWSSub, lggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		defer rpc.Close()
		require.NoError(t, rpc.Dial(ctx))

		_, sub, err := rpc.SubscribeToHeads(tests.Context(t))
		require.NoError(t, err)
		checkClosedRPCClientShouldRemoveExistingSub(t, ctx, sub, rpc)
	})
	t.Run("Closed rpc client should remove existing SubscribeToHeads subscription with HTTP polling", func(t *testing.T) {
		server := testutils.NewWSServer(t, chainId, serverCallBack)
		wsURL := server.WSURL()

		rpc := client.NewRPCClient(nodePoolCfgHeadPolling, lggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		defer rpc.Close()
		require.NoError(t, rpc.Dial(ctx))

		_, sub, err := rpc.SubscribeToHeads(tests.Context(t))
		require.NoError(t, err)
		checkClosedRPCClientShouldRemoveExistingSub(t, ctx, sub, rpc)
	})
	t.Run("Closed rpc client should remove existing SubscribeToFinalizedHeads subscription", func(t *testing.T) {
		server := testutils.NewWSServer(t, chainId, serverCallBack)
		wsURL := server.WSURL()

		rpc := client.NewRPCClient(nodePoolCfgHeadPolling, lggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		defer rpc.Close()
		require.NoError(t, rpc.Dial(ctx))

		_, sub, err := rpc.SubscribeToFinalizedHeads(tests.Context(t))
		require.NoError(t, err)
		checkClosedRPCClientShouldRemoveExistingSub(t, ctx, sub, rpc)
	})
	t.Run("Subscription error is properly wrapper", func(t *testing.T) {
		server := testutils.NewWSServer(t, chainId, serverCallBack)
		wsURL := server.WSURL()
		rpc := client.NewRPCClient(nodePoolCfgWSSub, lggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		defer rpc.Close()
		require.NoError(t, rpc.Dial(ctx))
		_, sub, err := rpc.SubscribeToHeads(ctx)
		require.NoError(t, err)
		go server.MustWriteBinaryMessageSync(t, "invalid msg")
		select {
		case err = <-sub.Err():
			require.ErrorContains(t, err, "RPCClient returned error (rpc): invalid character")
		case <-ctx.Done():
			t.Errorf("Expected subscription to return an error, but test timeout instead")
		}
	})
}

func TestRPCClient_SubscribeFilterLogs(t *testing.T) {
	t.Parallel()

	nodePoolCfg := client.TestNodePoolConfig{
		NodeNewHeadsPollInterval:       1 * time.Second,
		NodeFinalizedBlockPollInterval: 1 * time.Second,
	}

	chainId := big.NewInt(123456)
	lggr := logger.Test(t)
	ctx, cancel := context.WithTimeout(tests.Context(t), tests.WaitTimeout(t))
	defer cancel()
	t.Run("Failed SubscribeFilterLogs when WSURL is empty", func(t *testing.T) {
		// ws is optional when LogBroadcaster is disabled, however SubscribeFilterLogs will return error if ws is missing
		observedLggr := logger.Test(t)
		rpcClient := client.NewRPCClient(nodePoolCfg, observedLggr, nil, &url.URL{}, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		require.NoError(t, rpcClient.Dial(ctx))

		_, err := rpcClient.SubscribeFilterLogs(ctx, ethereum.FilterQuery{}, make(chan types.Log))
		require.Equal(t, errors.New("SubscribeFilterLogs is not allowed without ws url"), err)
	})
	t.Run("Failed SubscribeFilterLogs logs and returns proper error", func(t *testing.T) {
		server := testutils.NewWSServer(t, chainId, func(reqMethod string, reqParams gjson.Result) (resp testutils.JSONRPCResponse) {
			return resp
		})
		wsURL := server.WSURL()
		observedLggr, observed := logger.TestObserved(t, zap.DebugLevel)
		rpc := client.NewRPCClient(nodePoolCfg, observedLggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		require.NoError(t, rpc.Dial(ctx))
		server.Close()
		_, err := rpc.SubscribeFilterLogs(ctx, ethereum.FilterQuery{}, make(chan types.Log))
		require.ErrorContains(t, err, "RPCClient returned error (rpc)")
		tests.AssertLogEventually(t, observed, "evmclient.Client#SubscribeFilterLogs RPC call failure")
	})
	t.Run("Subscription error is properly wrapper", func(t *testing.T) {
		server := testutils.NewWSServer(t, chainId, func(method string, params gjson.Result) (resp testutils.JSONRPCResponse) {
			assert.Equal(t, "eth_subscribe", method)
			if assert.True(t, params.IsArray()) && assert.Equal(t, "logs", params.Array()[0].String()) {
				resp.Result = `"0x00"`
				resp.Notify = "{}"
			}
			return resp
		})
		wsURL := server.WSURL()
		rpc := client.NewRPCClient(nodePoolCfg, lggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
		defer rpc.Close()
		require.NoError(t, rpc.Dial(ctx))
		sub, err := rpc.SubscribeFilterLogs(ctx, ethereum.FilterQuery{}, make(chan types.Log))
		require.NoError(t, err)
		go server.MustWriteBinaryMessageSync(t, "invalid msg")
		errorCtx, cancel := context.WithTimeout(ctx, tests.DefaultWaitTimeout)
		defer cancel()
		select {
		case err = <-sub.Err():
			require.ErrorContains(t, err, "RPCClient returned error (rpc): invalid character")
		case <-errorCtx.Done():
			t.Errorf("Expected subscription to return an error, but test timeout instead")
		}
	})
	t.Run("Log's index is properly set for special chain types", func(t *testing.T) {
		chainTypes := []struct {
			Name      string
			ChainType chaintype.ChainType
		}{
			{Name: "Sei", ChainType: chaintype.ChainSei},
			{Name: "Hedera", ChainType: chaintype.ChainHedera},
			{Name: "Rootstock", ChainType: chaintype.ChainRootstock},
		}

		testCases := []struct {
			TxIndex       uint
			Index         uint
			ExpectedIndex uint
		}{
			{
				TxIndex:       0,
				Index:         0,
				ExpectedIndex: 0,
			},
			{
				TxIndex:       0,
				Index:         1,
				ExpectedIndex: 1,
			},
			{
				TxIndex:       1,
				Index:         0,
				ExpectedIndex: math.MaxUint32 + 1,
			},
		}

		for _, ct := range chainTypes {
			t.Run(ct.Name, func(t *testing.T) {
				server := testutils.NewWSServer(t, chainId, func(method string, params gjson.Result) (resp testutils.JSONRPCResponse) {
					if method == "eth_unsubscribe" {
						resp.Result = "true"
						return
					} else if method == "eth_subscribe" {
						if assert.True(t, params.IsArray()) && assert.Equal(t, "logs", params.Array()[0].String()) {
							resp.Result = `"0x00"`
						}
						return
					}
					return
				})
				wsURL := server.WSURL()
				rpc := client.NewRPCClient(nodePoolCfg, lggr, wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, ct.ChainType)
				defer rpc.Close()
				require.NoError(t, rpc.Dial(ctx))
				ch := make(chan types.Log)
				sub, err := rpc.SubscribeFilterLogs(ctx, ethereum.FilterQuery{}, ch)
				require.NoError(t, err)
				go func() {
					for _, testCase := range testCases {
						server.MustWriteBinaryMessageSync(t, makeNewWSMessage(types.Log{TxIndex: testCase.TxIndex, Index: testCase.Index, Topics: []common.Hash{{}}}))
					}
				}()
				defer sub.Unsubscribe()
				for _, testCase := range testCases {
					select {
					//nolint:staticcheck //SA1019 ignoring deprecated
					case <-tests.Context(t).Done():
						require.Fail(t, "context timed out")
					case err := <-sub.Err():
						require.NoError(t, err)
						require.Fail(t, "Did not expect error channel to be closed or return error before all testcases were consumed")
					case log := <-ch:
						require.Equal(t, testCase.ExpectedIndex, log.Index, "[%s] Unexpected log index %d for test case %v", ct.Name, log.Index, testCase)
					}
				}
			})
		}
	})
}

func TestRPCClientFilterLogs(t *testing.T) {
	t.Parallel()

	nodePoolCfg := client.TestNodePoolConfig{
		NodeNewHeadsPollInterval:       1 * time.Second,
		NodeFinalizedBlockPollInterval: 1 * time.Second,
	}

	chainID := big.NewInt(123456)
	lggr := logger.Test(t)
	ctx, cancel := context.WithTimeout(tests.Context(t), tests.WaitTimeout(t))
	defer cancel()
	t.Run("Log's index is properly set for special chain types", func(t *testing.T) {
		chainTypes := []struct {
			Name      string
			ChainType chaintype.ChainType
		}{
			{Name: "Sei", ChainType: chaintype.ChainSei},
			{Name: "Hedera", ChainType: chaintype.ChainHedera},
			{Name: "Rootstock", ChainType: chaintype.ChainRootstock},
		}

		testCases := []struct {
			TxIndex       uint
			Index         uint
			ExpectedIndex uint
		}{
			{
				TxIndex:       0,
				Index:         0,
				ExpectedIndex: 0,
			},
			{
				TxIndex:       0,
				Index:         1,
				ExpectedIndex: 1,
			},
			{
				TxIndex:       1,
				Index:         0,
				ExpectedIndex: math.MaxUint32 + 1,
			},
		}
		server := testutils.NewWSServer(t, chainID, func(method string, params gjson.Result) (resp testutils.JSONRPCResponse) {
			if method != "eth_getLogs" {
				return
			}
			var logs []types.Log
			for _, testCase := range testCases {
				logs = append(logs, types.Log{TxIndex: testCase.TxIndex, Index: testCase.Index, Topics: []common.Hash{{}}})
			}
			raw, err := json.Marshal(logs)
			require.NoError(t, err)
			resp.Result = string(raw)
			return
		})
		wsURL := server.WSURL()
		for _, ct := range chainTypes {
			t.Run(ct.Name, func(t *testing.T) {
				rpc := client.NewRPCClient(nodePoolCfg, lggr, wsURL, nil, "rpc", 1, chainID, multinode.Primary, client.QueryTimeout, client.QueryTimeout, ct.ChainType)
				defer rpc.Close()
				require.NoError(t, rpc.Dial(ctx))
				logs, err := rpc.FilterLogs(ctx, ethereum.FilterQuery{})
				require.NoError(t, err)
				for i, testCase := range testCases {
					require.Equal(t, testCase.ExpectedIndex, logs[i].Index, "Unexpected log index %d for test case %v", logs[i].Index, testCase)
				}
			})
		}

		t.Run("Other chains", func(t *testing.T) {
			// other networks should return index as is
			rpc := client.NewRPCClient(nodePoolCfg, lggr, wsURL, nil, "rpc", 1, chainID, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
			defer rpc.Close()
			require.NoError(t, rpc.Dial(ctx))
			logs, err := rpc.FilterLogs(ctx, ethereum.FilterQuery{})
			require.NoError(t, err)
			for i, testCase := range testCases {
				require.Equal(t, testCase.Index, logs[i].Index, "Expected other chains log to be returned as is")
				require.Equal(t, testCase.TxIndex, logs[i].TxIndex, "Expected other chains log to be returned as is")
			}
		})

	})
}

func TestRPCClient_LatestFinalizedBlock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(tests.Context(t), tests.WaitTimeout(t))
	defer cancel()

	nodePoolCfg := client.TestNodePoolConfig{
		NodeNewHeadsPollInterval:       1 * time.Second,
		NodeFinalizedBlockPollInterval: 1 * time.Second,
	}

	chainId := big.NewInt(123456)
	lggr := logger.Test(t)

	type rpcServer struct {
		Head atomic.Pointer[evmtypes.Head]
		URL  *url.URL
	}
	createRPCServer := func() *rpcServer {
		server := &rpcServer{}
		server.URL = testutils.NewWSServer(t, chainId, func(method string, params gjson.Result) (resp testutils.JSONRPCResponse) {
			assert.Equal(t, "eth_getBlockByNumber", method)
			if assert.True(t, params.IsArray()) && assert.Equal(t, "finalized", params.Array()[0].String()) {
				head := server.Head.Load()
				jsonHead, err := json.Marshal(head)
				if err != nil {
					panic(fmt.Errorf("failed to marshal head: %w", err))
				}
				resp.Result = string(jsonHead)
			}

			return
		}).WSURL()

		return server
	}

	server := createRPCServer()
	rpc := client.NewRPCClient(nodePoolCfg, lggr, server.URL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, "")
	require.NoError(t, rpc.Dial(ctx))
	defer rpc.Close()
	server.Head.Store(&evmtypes.Head{Number: 128})
	// updates chain info
	_, err := rpc.LatestFinalizedBlock(ctx)
	require.NoError(t, err)
	latest, highestUserObservations := rpc.GetInterceptedChainInfo()

	assert.Equal(t, int64(0), highestUserObservations.BlockNumber)
	assert.Equal(t, int64(128), highestUserObservations.FinalizedBlockNumber)

	assert.Equal(t, int64(0), latest.BlockNumber)
	assert.Equal(t, int64(128), latest.FinalizedBlockNumber)

	// lower block number does not update highestUserObservations
	server.Head.Store(&evmtypes.Head{Number: 127})
	_, err = rpc.LatestFinalizedBlock(ctx)
	require.NoError(t, err)
	latest, highestUserObservations = rpc.GetInterceptedChainInfo()

	assert.Equal(t, int64(0), highestUserObservations.BlockNumber)
	assert.Equal(t, int64(128), highestUserObservations.FinalizedBlockNumber)

	assert.Equal(t, int64(0), latest.BlockNumber)
	assert.Equal(t, int64(127), latest.FinalizedBlockNumber)

	// health check flg prevents change in highestUserObservations
	server.Head.Store(&evmtypes.Head{Number: 256})
	_, err = rpc.LatestFinalizedBlock(multinode.CtxAddHealthCheckFlag(ctx))
	require.NoError(t, err)
	latest, highestUserObservations = rpc.GetInterceptedChainInfo()

	assert.Equal(t, int64(0), highestUserObservations.BlockNumber)
	assert.Equal(t, int64(128), highestUserObservations.FinalizedBlockNumber)

	assert.Equal(t, int64(0), latest.BlockNumber)
	assert.Equal(t, int64(256), latest.FinalizedBlockNumber)

	// subscription updates chain info
	server.Head.Store(&evmtypes.Head{Number: 512})
	ch, sub, err := rpc.SubscribeToFinalizedHeads(ctx)
	require.NoError(t, err)
	defer sub.Unsubscribe()
	head := <-ch
	require.Equal(t, int64(512), head.BlockNumber())

	latest, highestUserObservations = rpc.GetInterceptedChainInfo()
	assert.Equal(t, int64(0), highestUserObservations.BlockNumber)
	assert.Equal(t, int64(512), highestUserObservations.FinalizedBlockNumber)

	assert.Equal(t, int64(0), latest.BlockNumber)
	assert.Equal(t, int64(512), latest.FinalizedBlockNumber)

	// health check subscription only updates latest
	sub.Unsubscribe() // close previous one
	server.Head.Store(&evmtypes.Head{Number: 1024})
	ch, sub, err = rpc.SubscribeToFinalizedHeads(multinode.CtxAddHealthCheckFlag(ctx))
	require.NoError(t, err)
	defer sub.Unsubscribe()
	head = <-ch
	require.Equal(t, int64(1024), head.BlockNumber())

	latest, highestUserObservations = rpc.GetInterceptedChainInfo()
	assert.Equal(t, int64(0), highestUserObservations.BlockNumber)
	assert.Equal(t, int64(512), highestUserObservations.FinalizedBlockNumber)

	assert.Equal(t, int64(0), latest.BlockNumber)
	assert.Equal(t, int64(1024), latest.FinalizedBlockNumber)

	// Close resets latest ChainInfo
	rpc.Close()
	latest, highestUserObservations = rpc.GetInterceptedChainInfo()
	assert.Equal(t, int64(0), highestUserObservations.BlockNumber)
	assert.Equal(t, int64(512), highestUserObservations.FinalizedBlockNumber)

	assert.Equal(t, int64(0), latest.BlockNumber)
	assert.Equal(t, int64(0), latest.FinalizedBlockNumber)
}

func TestRpcClientLargePayloadTimeout(t *testing.T) {
	t.Parallel()

	nodePoolCfg := client.TestNodePoolConfig{
		NodeNewHeadsPollInterval:       1 * time.Second,
		NodeFinalizedBlockPollInterval: 1 * time.Second,
	}

	testCases := []struct {
		Name string
		Fn   func(ctx context.Context, rpc *client.RPCClient) error
	}{
		{
			Name: "SendTransaction",
			Fn: func(ctx context.Context, rpc *client.RPCClient) error {
				_, _, err := rpc.SendTransaction(ctx, types.NewTx(&types.LegacyTx{}))
				return err
			},
		},
		{
			Name: "EstimateGas",
			Fn: func(ctx context.Context, rpc *client.RPCClient) error {
				_, err := rpc.EstimateGas(ctx, ethereum.CallMsg{})
				return err
			},
		},
		{
			Name: "CallContract",
			Fn: func(ctx context.Context, rpc *client.RPCClient) error {
				_, err := rpc.CallContract(ctx, ethereum.CallMsg{}, nil)
				return err
			},
		},
		{
			Name: "CallContext",
			Fn: func(ctx context.Context, rpc *client.RPCClient) error {
				err := rpc.CallContext(ctx, nil, "rpc_call", nil)
				return err
			},
		},
		{
			Name: "BatchCallContext",
			Fn: func(ctx context.Context, rpc *client.RPCClient) error {
				err := rpc.BatchCallContext(ctx, nil)
				return err
			},
		},
	}
	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			// use background context to ensure that the DeadlineExceeded is caused by timeout we've set on request
			// level, instead of one that was set on test level.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			chainId := big.NewInt(123456)
			rpcURL := testutils.NewWSServer(t, chainId, func(method string, params gjson.Result) (resp testutils.JSONRPCResponse) {
				// block until test is done
				<-ctx.Done()
				return
			}).WSURL()

			// use something unreasonably large for RPC timeout to ensure that we use largePayloadRPCTimeout
			const rpcTimeout = time.Hour
			const largePayloadRPCTimeout = tests.TestInterval
			rpc := client.NewRPCClient(nodePoolCfg, logger.Test(t), rpcURL, nil, "rpc", 1, chainId, multinode.Primary, largePayloadRPCTimeout, rpcTimeout, "")
			require.NoError(t, rpc.Dial(ctx))
			defer rpc.Close()
			err := testCase.Fn(ctx, rpc)
			assert.ErrorIs(t, err, context.DeadlineExceeded, "Expected DedlineExceeded error, but got: %v", err)
		})
	}
}

func TestRPCClient_Tron(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), tests.WaitTimeout(t))
	defer cancel()

	nodePoolCfg := client.TestNodePoolConfig{
		NodeNewHeadsPollInterval:       1 * time.Second,
		NodeFinalizedBlockPollInterval: 1 * time.Second,
	}

	chainID := big.NewInt(123456)
	lggr := logger.Test(t)

	// Create a server - though it should never be called for Tron
	server := testutils.NewWSServer(t, chainID, func(method string, params gjson.Result) (resp testutils.JSONRPCResponse) {
		assert.Fail(t, "Server should not be called for Tron methods")
		return resp
	})
	wsURL := server.WSURL()

	// Create the RPC client with Tron chain type
	rpc := client.NewRPCClient(nodePoolCfg, lggr, wsURL, nil, "rpc", 1, chainID, multinode.Primary, client.QueryTimeout, client.QueryTimeout, chaintype.ChainTron)
	defer rpc.Close()
	require.NoError(t, rpc.Dial(ctx))

	testAddr := common.HexToAddress("0x1234567890123456789012345678901234567890")

	t.Run("SendTransaction", func(t *testing.T) {
		// Create a simple transaction
		tx := types.NewTx(&types.LegacyTx{
			Nonce:    0,
			GasPrice: big.NewInt(1000000000),
			Gas:      21000,
			To:       &common.Address{},
			Value:    big.NewInt(1),
			Data:     nil,
		})

		// Call SendTransaction
		_, _, err := rpc.SendTransaction(ctx, tx)

		// Verify it returns the expected error for Tron
		require.Error(t, err)
		assert.Equal(t, "SendTransaction not implemented for Tron, this should never be called", err.Error())
	})

	t.Run("NonceAt", func(t *testing.T) {
		// Call NonceAt with a test address
		_, err := rpc.NonceAt(ctx, testAddr, nil)

		// Verify it returns an error
		require.Error(t, err, "tron does not support eth_getTransactionCount")
	})

	t.Run("PendingSequenceAt", func(t *testing.T) {
		// Call PendingSequenceAt with a test address
		_, err := rpc.PendingSequenceAt(ctx, testAddr)

		// Verify it returns an error
		require.Error(t, err, "tron does not support eth_getTransactionCount")
	})
}

func TestAstarCustomFinality(t *testing.T) {
	t.Parallel()

	nodePoolCfg := client.TestNodePoolConfig{
		NodeNewHeadsPollInterval:       1 * time.Second,
		NodeFinalizedBlockPollInterval: 1 * time.Second,
	}

	chainId := big.NewInt(123456)
	// create new server that returns 4 block for Astar custom finality and 8 block for finality tag.
	wsURL := testutils.NewWSServer(t, chainId, func(method string, params gjson.Result) (resp testutils.JSONRPCResponse) {
		switch method {
		case "chain_getFinalizedHead":
			resp.Result = `"0xf14c499253fd7bbcba142e5dd77dad8b5ad598c1dc414a66bacdd8dae14a6759"`
		case "chain_getHeader":
			if assert.True(t, params.IsArray()) && assert.Equal(t, "0xf14c499253fd7bbcba142e5dd77dad8b5ad598c1dc414a66bacdd8dae14a6759", params.Array()[0].String()) {
				resp.Result = `{"parentHash":"0x1311773bc6b4efc8f438ed1f094524b2a1233baf8a35396f641fcc42a378fc62","number":"0x4","stateRoot":"0x0e4920dc5516b587e1f74a0b65963134523a12cc11478bb314e52895758fbfa2","extrinsicsRoot":"0x5b02446dcab0659eb07d4a38f28f181c1b78a71b2aba207bb0ea1f0f3468e6bd","digest":{"logs":["0x066175726120ad678e0800000000","0x04525053529023158dc8e8fd0180bf26d88233a3d94eed2f4e43480395f0809f28791965e4d34e9b3905","0x0466726f6e88017441e97acf83f555e0deefef86db636bc8a37eb84747603412884e4df4d2280400","0x056175726101018a0a57edf70cc5474323114a47ee1e7f645b8beea5a1560a996416458e89f42bdf4955e24d32b5da54e1bf628aaa7ce4b8c0fa2b95c175a139d88786af12a88c"]}}`
			}
		case "eth_getBlockByNumber":
			assert.True(t, params.IsArray())
			switch params.Array()[0].String() {
			case "0x4":
				resp.Result = `{"author":"0x5accb3bf9194a5f81b2087d4bd6ac47c62775d49","baseFeePerGas":"0xb576270823","difficulty":"0x0","extraData":"0x","gasLimit":"0xe4e1c0","gasUsed":"0x0","hash":"0x7441e97acf83f555e0deefef86db636bc8a37eb84747603412884e4df4d22804","logsBloom":"0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000","miner":"0x5accb3bf9194a5f81b2087d4bd6ac47c62775d49","nonce":"0x0000000000000000","number":"0x4","parentHash":"0x6ba069c318b692bf2cc0bd7ea070a9382a20c2f52413c10554b57c2e381bf2bb","receiptsRoot":"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421","sha3Uncles":"0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347","size":"0x201","stateRoot":"0x17c46d359b9af773312c747f1d20032c67658d9a2923799f00533b73789cf49b","timestamp":"0x66acdc22","totalDifficulty":"0x0","transactions":[],"transactionsRoot":"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421","uncles":[]}`
			case "finalized":
				resp.Result = `{"author":"0x1687736326c9fea17e25fc5287613693c912909c","baseFeePerGas":"0x3b9aca00","difficulty":"0x0","extraData":"0x","gasLimit":"0xe4e1c0","gasUsed":"0x0","hash":"0x62f03413681948b06882e7d9f91c4949bc39ded98d36336ab03faea038ec8e3d","logsBloom":"0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000","miner":"0x1687736326c9fea17e25fc5287613693c912909c","nonce":"0x0000000000000000","number":"0x8","parentHash":"0x43f504afdc639cbb8daf5fd5328a37762164b73f9c70ed54e1928c1fca6d8f23","receiptsRoot":"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421","sha3Uncles":"0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347","size":"0x200","stateRoot":"0x0cb938d51ad83bdf401e3f5f7f989e60df64fdea620d394af41a3e72629f7495","timestamp":"0x61bd8d1a","totalDifficulty":"0x0","transactions":[],"transactionsRoot":"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421","uncles":[]}`
			default:
				assert.Fail(t, fmt.Sprintf("unexpected eth_getBlockByNumber param: %v", params.Array()))
			}
		default:
			assert.Fail(t, "unexpected method: "+method)
		}
		return
	}).WSURL()

	const expectedFinalizedBlockNumber = int64(4)
	const expectedFinalizedBlockHash = "0x7441e97acf83f555e0deefef86db636bc8a37eb84747603412884e4df4d22804"
	rpcClient := client.NewRPCClient(nodePoolCfg, logger.Test(t), wsURL, nil, "rpc", 1, chainId, multinode.Primary, client.QueryTimeout, client.QueryTimeout, chaintype.ChainAstar)
	defer rpcClient.Close()
	err := rpcClient.Dial(tests.Context(t))
	require.NoError(t, err)

	testCases := []struct {
		Name               string
		GetLatestFinalized func(ctx context.Context) (*evmtypes.Head, error)
	}{
		{
			Name: "Direct LatestFinalized call",
			GetLatestFinalized: func(ctx context.Context) (*evmtypes.Head, error) {
				return rpcClient.LatestFinalizedBlock(ctx)
			},
		},
		{
			Name: "BatchCallContext with Finalized tag as string",
			GetLatestFinalized: func(ctx context.Context) (*evmtypes.Head, error) {
				result := &evmtypes.Head{}
				req := rpc.BatchElem{
					Method: "eth_getBlockByNumber",
					Args:   []interface{}{rpc.FinalizedBlockNumber.String(), false},
					Result: result,
				}
				err := rpcClient.BatchCallContext(ctx, []rpc.BatchElem{
					req,
				})
				if err != nil {
					return nil, err
				}

				return result, req.Error
			},
		},
		{
			Name: "BatchCallContext with Finalized tag as BlockNumber",
			GetLatestFinalized: func(ctx context.Context) (*evmtypes.Head, error) {
				result := &evmtypes.Head{}
				req := rpc.BatchElem{
					Method: "eth_getBlockByNumber",
					Args:   []interface{}{rpc.FinalizedBlockNumber, false},
					Result: result,
				}
				err := rpcClient.BatchCallContext(ctx, []rpc.BatchElem{req})
				if err != nil {
					return nil, err
				}

				return result, req.Error
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.Name, func(t *testing.T) {
			lf, err := testCase.GetLatestFinalized(tests.Context(t))
			require.NoError(t, err)
			require.NotNil(t, lf)
			assert.Equal(t, expectedFinalizedBlockHash, lf.Hash.String())
			assert.Equal(t, expectedFinalizedBlockNumber, lf.Number)
		})
	}
}
