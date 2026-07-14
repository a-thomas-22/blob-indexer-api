package ethereum

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

type blobScheduleWire struct {
	Target         int    `json:"target"`
	Max            int    `json:"max"`
	UpdateFraction uint64 `json:"baseFeeUpdateFraction"`
}

type forkWire struct {
	ActivationTime uint64            `json:"activationTime"`
	BlobSchedule   *blobScheduleWire `json:"blobSchedule"`
}

type ethConfigWire struct {
	Current *forkWire `json:"current"`
	Next    *forkWire `json:"next"`
	Last    *forkWire `json:"last"`
}

// configEthService serves eth_config for the in-process RPC server.
type configEthService struct {
	resp *ethConfigWire
	err  error
}

func (s *configEthService) Config(_ context.Context) (*ethConfigWire, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func newConfigClient(t *testing.T, svc *configEthService) *Client {
	t.Helper()
	srv := rpc.NewServer()
	if err := srv.RegisterName("eth", svc); err != nil {
		t.Fatalf("register eth service: %v", err)
	}
	rpcClient := rpc.DialInProc(srv)
	t.Cleanup(rpcClient.Close)
	return &Client{
		ethClient:     ethclient.NewClient(rpcClient),
		rpcClient:     rpcClient,
		blockSubs:     make(map[string]*BlockSubscription),
		pendingTxSubs: make(map[string]*PendingTxSubscription),
	}
}

func TestFetchEthConfig_ParsesCurrentNextLast(t *testing.T) {
	svc := &configEthService{resp: &ethConfigWire{
		Current: &forkWire{ActivationTime: 1762955544, BlobSchedule: &blobScheduleWire{Target: 14, Max: 21, UpdateFraction: 11684671}},
		Next:    &forkWire{ActivationTime: 1763545368, BlobSchedule: &blobScheduleWire{Target: 21, Max: 32, UpdateFraction: 20609697}},
		Last:    &forkWire{ActivationTime: 1762955544, BlobSchedule: &blobScheduleWire{Target: 14, Max: 21, UpdateFraction: 11684671}},
	}}
	client := newConfigClient(t, svc)

	got, err := client.FetchEthConfig(context.Background())
	if err != nil {
		t.Fatalf("FetchEthConfig() error = %v", err)
	}
	if got.Current == nil || got.Current.ActivationTime != 1762955544 {
		t.Fatalf("current activation = %+v, want 1762955544", got.Current)
	}
	if got.Current.BlobSchedule == nil || got.Current.BlobSchedule.Target != 14 || got.Current.BlobSchedule.Max != 21 {
		t.Errorf("current blob schedule = %+v, want target 14 / max 21", got.Current.BlobSchedule)
	}
	if got.Current.BlobSchedule.UpdateFraction != 11684671 {
		t.Errorf("current update fraction = %d, want 11684671", got.Current.BlobSchedule.UpdateFraction)
	}
	if got.Next == nil || got.Next.BlobSchedule.Target != 21 {
		t.Errorf("next target = %+v, want 21", got.Next)
	}
}

func TestFetchEthConfig_NodeError(t *testing.T) {
	client := newConfigClient(t, &configEthService{err: errors.New("method not supported")})
	if _, err := client.FetchEthConfig(context.Background()); err == nil {
		t.Fatal("expected error when node rejects eth_config")
	}
}
