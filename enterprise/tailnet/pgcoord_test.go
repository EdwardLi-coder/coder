package tailnet_test

import (
	"context"
	"database/sql"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/mock/gomock"
	"golang.org/x/exp/slices"
	"golang.org/x/xerrors"
	gProto "google.golang.org/protobuf/proto"

	"cdr.dev/slog"
	"cdr.dev/slog/sloggers/slogtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbmock"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/pubsub"
	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/coder/v2/enterprise/tailnet"
	agpl "github.com/coder/coder/v2/tailnet"
	"github.com/coder/coder/v2/tailnet/proto"
	agpltest "github.com/coder/coder/v2/tailnet/test"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestPGCoordinatorSingle_ClientWithoutAgent(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()

	agentID := uuid.New()
	client := newTestClient(t, coordinator, agentID)
	defer client.close()
	client.sendNode(&agpl.Node{PreferredDERP: 10})
	require.Eventually(t, func() bool {
		clients, err := store.GetTailnetTunnelPeerBindings(ctx, agentID)
		if err != nil && !xerrors.Is(err, sql.ErrNoRows) {
			t.Fatalf("database error: %v", err)
		}
		if len(clients) == 0 {
			return false
		}
		node := new(proto.Node)
		err = gProto.Unmarshal(clients[0].Node, node)
		assert.NoError(t, err)
		assert.EqualValues(t, 10, node.PreferredDerp)
		return true
	}, testutil.WaitShort, testutil.IntervalFast)

	err = client.close()
	require.NoError(t, err)
	<-client.errChan
	<-client.closeChan
	assertEventuallyLost(ctx, t, store, client.id)
}

func TestPGCoordinatorSingle_AgentWithoutClients(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()

	agent := newTestAgent(t, coordinator, "agent")
	defer agent.close()
	agent.sendNode(&agpl.Node{PreferredDERP: 10})
	require.Eventually(t, func() bool {
		agents, err := store.GetTailnetPeers(ctx, agent.id)
		if err != nil && !xerrors.Is(err, sql.ErrNoRows) {
			t.Fatalf("database error: %v", err)
		}
		if len(agents) == 0 {
			return false
		}
		node := new(proto.Node)
		err = gProto.Unmarshal(agents[0].Node, node)
		assert.NoError(t, err)
		assert.EqualValues(t, 10, node.PreferredDerp)
		return true
	}, testutil.WaitShort, testutil.IntervalFast)
	err = agent.close()
	require.NoError(t, err)
	<-agent.errChan
	<-agent.closeChan
	assertEventuallyLost(ctx, t, store, agent.id)
}

func TestPGCoordinatorSingle_AgentInvalidIP(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()

	agent := newTestAgent(t, coordinator, "agent")
	defer agent.close()
	agent.sendNode(&agpl.Node{
		Addresses: []netip.Prefix{
			netip.PrefixFrom(agpl.IP(), 128),
		},
		PreferredDERP: 10,
	})

	// The agent connection should be closed immediately after sending an invalid addr
	testutil.RequireRecvCtx(ctx, t, agent.closeChan)
	assertEventuallyLost(ctx, t, store, agent.id)
}

func TestPGCoordinatorSingle_AgentInvalidIPBits(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()

	agent := newTestAgent(t, coordinator, "agent")
	defer agent.close()
	agent.sendNode(&agpl.Node{
		Addresses: []netip.Prefix{
			netip.PrefixFrom(agpl.IPFromUUID(agent.id), 64),
		},
		PreferredDERP: 10,
	})

	// The agent connection should be closed immediately after sending an invalid addr
	testutil.RequireRecvCtx(ctx, t, agent.closeChan)
	assertEventuallyLost(ctx, t, store, agent.id)
}

func TestPGCoordinatorSingle_AgentValidIP(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()

	agent := newTestAgent(t, coordinator, "agent")
	defer agent.close()
	agent.sendNode(&agpl.Node{
		Addresses: []netip.Prefix{
			netip.PrefixFrom(agpl.IPFromUUID(agent.id), 128),
		},
		PreferredDERP: 10,
	})
	require.Eventually(t, func() bool {
		agents, err := store.GetTailnetPeers(ctx, agent.id)
		if err != nil && !xerrors.Is(err, sql.ErrNoRows) {
			t.Fatalf("database error: %v", err)
		}
		if len(agents) == 0 {
			return false
		}
		node := new(proto.Node)
		err = gProto.Unmarshal(agents[0].Node, node)
		assert.NoError(t, err)
		assert.EqualValues(t, 10, node.PreferredDerp)
		return true
	}, testutil.WaitShort, testutil.IntervalFast)
	err = agent.close()
	require.NoError(t, err)
	<-agent.errChan
	<-agent.closeChan
	assertEventuallyLost(ctx, t, store, agent.id)
}

func TestPGCoordinatorSingle_AgentValidIPLegacy(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()

	agent := newTestAgent(t, coordinator, "agent")
	defer agent.close()
	agent.sendNode(&agpl.Node{
		Addresses: []netip.Prefix{
			netip.PrefixFrom(workspacesdk.AgentIP, 128),
		},
		PreferredDERP: 10,
	})
	require.Eventually(t, func() bool {
		agents, err := store.GetTailnetPeers(ctx, agent.id)
		if err != nil && !xerrors.Is(err, sql.ErrNoRows) {
			t.Fatalf("database error: %v", err)
		}
		if len(agents) == 0 {
			return false
		}
		node := new(proto.Node)
		err = gProto.Unmarshal(agents[0].Node, node)
		assert.NoError(t, err)
		assert.EqualValues(t, 10, node.PreferredDerp)
		return true
	}, testutil.WaitShort, testutil.IntervalFast)
	err = agent.close()
	require.NoError(t, err)
	<-agent.errChan
	<-agent.closeChan
	assertEventuallyLost(ctx, t, store, agent.id)
}

func TestPGCoordinatorSingle_AgentWithClient(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()

	agent := newTestAgent(t, coordinator, "original")
	defer agent.close()
	agent.sendNode(&agpl.Node{PreferredDERP: 10})

	client := newTestClient(t, coordinator, agent.id)
	defer client.close()

	agentNodes := client.recvNodes(ctx, t)
	require.Len(t, agentNodes, 1)
	assert.Equal(t, 10, agentNodes[0].PreferredDERP)
	client.sendNode(&agpl.Node{PreferredDERP: 11})
	clientNodes := agent.recvNodes(ctx, t)
	require.Len(t, clientNodes, 1)
	assert.Equal(t, 11, clientNodes[0].PreferredDERP)

	// Ensure an update to the agent node reaches the connIO!
	agent.sendNode(&agpl.Node{PreferredDERP: 12})
	agentNodes = client.recvNodes(ctx, t)
	require.Len(t, agentNodes, 1)
	assert.Equal(t, 12, agentNodes[0].PreferredDERP)

	// Close the agent WebSocket so a new one can connect.
	err = agent.close()
	require.NoError(t, err)
	_ = agent.recvErr(ctx, t)
	agent.waitForClose(ctx, t)

	// Create a new agent connection. This is to simulate a reconnect!
	agent = newTestAgent(t, coordinator, "reconnection", agent.id)
	// Ensure the existing listening connIO sends its node immediately!
	clientNodes = agent.recvNodes(ctx, t)
	require.Len(t, clientNodes, 1)
	assert.Equal(t, 11, clientNodes[0].PreferredDERP)

	// Send a bunch of updates in rapid succession, and test that we eventually get the latest.  We don't want the
	// coordinator accidentally reordering things.
	for d := 13; d < 36; d++ {
		agent.sendNode(&agpl.Node{PreferredDERP: d})
	}
	for {
		nodes := client.recvNodes(ctx, t)
		if !assert.Len(t, nodes, 1) {
			break
		}
		if nodes[0].PreferredDERP == 35 {
			// got latest!
			break
		}
	}

	err = agent.close()
	require.NoError(t, err)
	_ = agent.recvErr(ctx, t)
	agent.waitForClose(ctx, t)

	err = client.close()
	require.NoError(t, err)
	_ = client.recvErr(ctx, t)
	client.waitForClose(ctx, t)

	assertEventuallyLost(ctx, t, store, agent.id)
	assertEventuallyLost(ctx, t, store, client.id)
}

func TestPGCoordinatorSingle_MissedHeartbeats(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitShort)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	mClock := quartz.NewMock(t)
	afTrap := mClock.Trap().AfterFunc("heartbeats", "recvBeat")
	defer afTrap.Close()
	rstTrap := mClock.Trap().TimerReset("heartbeats", "resetExpiryTimerWithLock")
	defer rstTrap.Close()

	coordinator, err := tailnet.NewTestPGCoord(ctx, logger, ps, store, mClock)
	require.NoError(t, err)
	defer coordinator.Close()

	agent := newTestAgent(t, coordinator, "agent")
	defer agent.close()
	agent.sendNode(&agpl.Node{PreferredDERP: 10})

	client := newTestClient(t, coordinator, agent.id)
	defer client.close()

	assertEventuallyHasDERPs(ctx, t, client, 10)
	client.sendNode(&agpl.Node{PreferredDERP: 11})
	assertEventuallyHasDERPs(ctx, t, agent, 11)

	// simulate a second coordinator via DB calls only --- our goal is to test broken heart-beating, so we can't use a
	// real coordinator
	fCoord2 := &fakeCoordinator{
		ctx:   ctx,
		t:     t,
		store: store,
		id:    uuid.New(),
	}

	fCoord2.heartbeat()
	afTrap.MustWait(ctx).Release() // heartbeat timeout started

	fCoord2.agentNode(agent.id, &agpl.Node{PreferredDERP: 12})
	assertEventuallyHasDERPs(ctx, t, client, 12)

	fCoord3 := &fakeCoordinator{
		ctx:   ctx,
		t:     t,
		store: store,
		id:    uuid.New(),
	}
	fCoord3.heartbeat()
	rstTrap.MustWait(ctx).Release() // timeout gets reset
	fCoord3.agentNode(agent.id, &agpl.Node{PreferredDERP: 13})
	assertEventuallyHasDERPs(ctx, t, client, 13)

	// fCoord2 sends in a second heartbeat, one period later (on time)
	mClock.Advance(tailnet.HeartbeatPeriod).MustWait(ctx)
	fCoord2.heartbeat()
	rstTrap.MustWait(ctx).Release() // timeout gets reset

	// when the fCoord3 misses enough heartbeats, the real coordinator should send an update with the
	// node from fCoord2 for the agent.
	mClock.Advance(tailnet.HeartbeatPeriod).MustWait(ctx)
	w := mClock.Advance(tailnet.HeartbeatPeriod)
	rstTrap.MustWait(ctx).Release()
	w.MustWait(ctx)
	assertEventuallyHasDERPs(ctx, t, client, 12)

	// one more heartbeat period will result in fCoord2 being expired, which should cause us to
	// revert to the original agent mapping
	mClock.Advance(tailnet.HeartbeatPeriod).MustWait(ctx)
	// note that the timeout doesn't get reset because both fCoord2 and fCoord3 are expired
	assertEventuallyHasDERPs(ctx, t, client, 10)

	// send fCoord3 heartbeat, which should trigger us to consider that mapping valid again.
	fCoord3.heartbeat()
	rstTrap.MustWait(ctx).Release() // timeout gets reset
	assertEventuallyHasDERPs(ctx, t, client, 13)

	err = agent.close()
	require.NoError(t, err)
	_ = agent.recvErr(ctx, t)
	agent.waitForClose(ctx, t)

	err = client.close()
	require.NoError(t, err)
	_ = client.recvErr(ctx, t)
	client.waitForClose(ctx, t)

	assertEventuallyLost(ctx, t, store, client.id)
}

func TestPGCoordinatorSingle_MissedHeartbeats_NoDrop(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)

	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()

	agentID := uuid.New()

	client := agpltest.NewPeer(ctx, t, coordinator, "client")
	defer client.Close(ctx)
	client.AddTunnel(agentID)

	client.UpdateDERP(11)

	// simulate a second coordinator via DB calls only --- our goal is to test
	// broken heart-beating, so we can't use a real coordinator
	fCoord2 := &fakeCoordinator{
		ctx:   ctx,
		t:     t,
		store: store,
		id:    uuid.New(),
	}
	// simulate a single heartbeat, the coordinator is healthy
	fCoord2.heartbeat()

	fCoord2.agentNode(agentID, &agpl.Node{PreferredDERP: 12})
	// since it's healthy the client should get the new node.
	client.AssertEventuallyHasDERP(agentID, 12)

	// the heartbeat should then timeout and we'll get sent a LOST update, NOT a
	// disconnect.
	client.AssertEventuallyLost(agentID)

	client.Close(ctx)

	assertEventuallyLost(ctx, t, store, client.ID)
}

func TestPGCoordinatorSingle_SendsHeartbeats(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)

	mu := sync.Mutex{}
	heartbeats := []time.Time{}
	unsub, err := ps.SubscribeWithErr(tailnet.EventHeartbeats, func(_ context.Context, _ []byte, err error) {
		assert.NoError(t, err)
		mu.Lock()
		defer mu.Unlock()
		heartbeats = append(heartbeats, time.Now())
	})
	require.NoError(t, err)
	defer unsub()

	start := time.Now()
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(heartbeats) < 2 {
			return false
		}
		require.Greater(t, heartbeats[0].Sub(start), time.Duration(0))
		require.Greater(t, heartbeats[1].Sub(start), time.Duration(0))
		return assert.Greater(t, heartbeats[1].Sub(heartbeats[0]), tailnet.HeartbeatPeriod*9/10)
	}, testutil.WaitMedium, testutil.IntervalMedium)
}

// TestPGCoordinatorDual_Mainline tests with 2 coordinators, one agent connected to each, and 2 clients per agent.
//
//	            +---------+
//	agent1 ---> | coord1  | <--- client11 (coord 1, agent 1)
//	            |         |
//	            |         | <--- client12 (coord 1, agent 2)
//	            +---------+
//	            +---------+
//	agent2 ---> | coord2  | <--- client21 (coord 2, agent 1)
//	            |         |
//	            |         | <--- client22 (coord2, agent 2)
//	            +---------+
func TestPGCoordinatorDual_Mainline(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coord1, err := tailnet.NewPGCoord(ctx, logger.Named("coord1"), ps, store)
	require.NoError(t, err)
	defer coord1.Close()
	coord2, err := tailnet.NewPGCoord(ctx, logger.Named("coord2"), ps, store)
	require.NoError(t, err)
	defer coord2.Close()

	agent1 := newTestAgent(t, coord1, "agent1")
	defer agent1.close()
	t.Logf("agent1=%s", agent1.id)
	agent2 := newTestAgent(t, coord2, "agent2")
	defer agent2.close()
	t.Logf("agent2=%s", agent2.id)

	client11 := newTestClient(t, coord1, agent1.id)
	defer client11.close()
	t.Logf("client11=%s", client11.id)
	client12 := newTestClient(t, coord1, agent2.id)
	defer client12.close()
	t.Logf("client12=%s", client12.id)
	client21 := newTestClient(t, coord2, agent1.id)
	defer client21.close()
	t.Logf("client21=%s", client21.id)
	client22 := newTestClient(t, coord2, agent2.id)
	defer client22.close()
	t.Logf("client22=%s", client22.id)

	t.Logf("client11 -> Node 11")
	client11.sendNode(&agpl.Node{PreferredDERP: 11})
	assertEventuallyHasDERPs(ctx, t, agent1, 11)

	t.Logf("client21 -> Node 21")
	client21.sendNode(&agpl.Node{PreferredDERP: 21})
	assertEventuallyHasDERPs(ctx, t, agent1, 21)

	t.Logf("client22 -> Node 22")
	client22.sendNode(&agpl.Node{PreferredDERP: 22})
	assertEventuallyHasDERPs(ctx, t, agent2, 22)

	t.Logf("agent2 -> Node 2")
	agent2.sendNode(&agpl.Node{PreferredDERP: 2})
	assertEventuallyHasDERPs(ctx, t, client22, 2)
	assertEventuallyHasDERPs(ctx, t, client12, 2)

	t.Logf("client12 -> Node 12")
	client12.sendNode(&agpl.Node{PreferredDERP: 12})
	assertEventuallyHasDERPs(ctx, t, agent2, 12)

	t.Logf("agent1 -> Node 1")
	agent1.sendNode(&agpl.Node{PreferredDERP: 1})
	assertEventuallyHasDERPs(ctx, t, client21, 1)
	assertEventuallyHasDERPs(ctx, t, client11, 1)

	t.Logf("close coord2")
	err = coord2.Close()
	require.NoError(t, err)

	// this closes agent2, client22, client21
	err = agent2.recvErr(ctx, t)
	require.ErrorIs(t, err, io.EOF)
	err = client22.recvErr(ctx, t)
	require.ErrorIs(t, err, io.EOF)
	err = client21.recvErr(ctx, t)
	require.ErrorIs(t, err, io.EOF)
	assertEventuallyLost(ctx, t, store, agent2.id)
	assertEventuallyLost(ctx, t, store, client21.id)
	assertEventuallyLost(ctx, t, store, client22.id)

	err = coord1.Close()
	require.NoError(t, err)
	// this closes agent1, client12, client11
	err = agent1.recvErr(ctx, t)
	require.ErrorIs(t, err, io.EOF)
	err = client12.recvErr(ctx, t)
	require.ErrorIs(t, err, io.EOF)
	err = client11.recvErr(ctx, t)
	require.ErrorIs(t, err, io.EOF)
	assertEventuallyLost(ctx, t, store, agent1.id)
	assertEventuallyLost(ctx, t, store, client11.id)
	assertEventuallyLost(ctx, t, store, client12.id)

	// wait for all connections to close
	err = agent1.close()
	require.NoError(t, err)
	agent1.waitForClose(ctx, t)

	err = agent2.close()
	require.NoError(t, err)
	agent2.waitForClose(ctx, t)

	err = client11.close()
	require.NoError(t, err)
	client11.waitForClose(ctx, t)

	err = client12.close()
	require.NoError(t, err)
	client12.waitForClose(ctx, t)

	err = client21.close()
	require.NoError(t, err)
	client21.waitForClose(ctx, t)

	err = client22.close()
	require.NoError(t, err)
	client22.waitForClose(ctx, t)
}

// TestPGCoordinator_MultiCoordinatorAgent tests when a single agent connects to multiple coordinators.
// We use two agent connections, but they share the same AgentID.  This could happen due to a reconnection,
// or an infrastructure problem where an old workspace is not fully cleaned up before a new one started.
//
//	            +---------+
//	agent1 ---> | coord1  |
//	            +---------+
//	            +---------+
//	agent2 ---> | coord2  |
//	            +---------+
//	            +---------+
//	            | coord3  | <--- client
//	            +---------+
func TestPGCoordinator_MultiCoordinatorAgent(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coord1, err := tailnet.NewPGCoord(ctx, logger.Named("coord1"), ps, store)
	require.NoError(t, err)
	defer coord1.Close()
	coord2, err := tailnet.NewPGCoord(ctx, logger.Named("coord2"), ps, store)
	require.NoError(t, err)
	defer coord2.Close()
	coord3, err := tailnet.NewPGCoord(ctx, logger.Named("coord3"), ps, store)
	require.NoError(t, err)
	defer coord3.Close()

	agent1 := newTestAgent(t, coord1, "agent1")
	defer agent1.close()
	agent2 := newTestAgent(t, coord2, "agent2", agent1.id)
	defer agent2.close()

	client := newTestClient(t, coord3, agent1.id)
	defer client.close()

	client.sendNode(&agpl.Node{PreferredDERP: 3})
	assertEventuallyHasDERPs(ctx, t, agent1, 3)
	assertEventuallyHasDERPs(ctx, t, agent2, 3)

	agent1.sendNode(&agpl.Node{PreferredDERP: 1})
	assertEventuallyHasDERPs(ctx, t, client, 1)

	// agent2's update overrides agent1 because it is newer
	agent2.sendNode(&agpl.Node{PreferredDERP: 2})
	assertEventuallyHasDERPs(ctx, t, client, 2)

	// agent2 disconnects, and we should revert back to agent1
	err = agent2.close()
	require.NoError(t, err)
	err = agent2.recvErr(ctx, t)
	require.ErrorIs(t, err, io.ErrClosedPipe)
	agent2.waitForClose(ctx, t)
	assertEventuallyHasDERPs(ctx, t, client, 1)

	agent1.sendNode(&agpl.Node{PreferredDERP: 11})
	assertEventuallyHasDERPs(ctx, t, client, 11)

	client.sendNode(&agpl.Node{PreferredDERP: 31})
	assertEventuallyHasDERPs(ctx, t, agent1, 31)

	err = agent1.close()
	require.NoError(t, err)
	err = agent1.recvErr(ctx, t)
	require.ErrorIs(t, err, io.ErrClosedPipe)
	agent1.waitForClose(ctx, t)

	err = client.close()
	require.NoError(t, err)
	err = client.recvErr(ctx, t)
	require.ErrorIs(t, err, io.ErrClosedPipe)
	client.waitForClose(ctx, t)

	assertEventuallyLost(ctx, t, store, client.id)
	assertEventuallyLost(ctx, t, store, agent1.id)
}

func TestPGCoordinator_Unhealthy(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	ctrl := gomock.NewController(t)
	mStore := dbmock.NewMockStore(ctrl)
	ps := pubsub.NewInMemory()
	logger := slogtest.Make(t, &slogtest.Options{IgnoreErrors: true}).Leveled(slog.LevelDebug)

	calls := make(chan struct{})
	threeMissed := mStore.EXPECT().UpsertTailnetCoordinator(gomock.Any(), gomock.Any()).
		Times(3).
		Do(func(_ context.Context, _ uuid.UUID) { <-calls }).
		Return(database.TailnetCoordinator{}, xerrors.New("test disconnect"))
	mStore.EXPECT().UpsertTailnetCoordinator(gomock.Any(), gomock.Any()).
		MinTimes(1).
		After(threeMissed).
		Do(func(_ context.Context, _ uuid.UUID) { <-calls }).
		Return(database.TailnetCoordinator{}, nil)
	// extra calls we don't particularly care about for this test
	mStore.EXPECT().CleanTailnetCoordinators(gomock.Any()).AnyTimes().Return(nil)
	mStore.EXPECT().CleanTailnetLostPeers(gomock.Any()).AnyTimes().Return(nil)
	mStore.EXPECT().CleanTailnetTunnels(gomock.Any()).AnyTimes().Return(nil)
	mStore.EXPECT().GetTailnetTunnelPeerIDs(gomock.Any(), gomock.Any()).AnyTimes().Return(nil, nil)
	mStore.EXPECT().GetTailnetTunnelPeerBindings(gomock.Any(), gomock.Any()).
		AnyTimes().Return(nil, nil)
	mStore.EXPECT().DeleteTailnetPeer(gomock.Any(), gomock.Any()).
		AnyTimes().Return(database.DeleteTailnetPeerRow{}, nil)
	mStore.EXPECT().DeleteAllTailnetTunnels(gomock.Any(), gomock.Any()).AnyTimes().Return(nil)
	mStore.EXPECT().UpdateTailnetPeerStatusByCoordinator(gomock.Any(), gomock.Any())

	uut, err := tailnet.NewPGCoord(ctx, logger, ps, mStore)
	require.NoError(t, err)
	defer func() {
		err := uut.Close()
		require.NoError(t, err)
	}()
	agent1 := newTestAgent(t, uut, "agent1")
	defer agent1.close()
	for i := 0; i < 3; i++ {
		select {
		case <-ctx.Done():
			t.Fatal("timeout")
		case calls <- struct{}{}:
			// OK
		}
	}
	// connected agent should be disconnected
	agent1.waitForClose(ctx, t)

	// new agent should immediately disconnect
	agent2 := newTestAgent(t, uut, "agent2")
	defer agent2.close()
	agent2.waitForClose(ctx, t)

	// next heartbeats succeed, so we are healthy
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.Done():
			t.Fatal("timeout")
		case calls <- struct{}{}:
			// OK
		}
	}
	agent3 := newTestAgent(t, uut, "agent3")
	defer agent3.close()
	select {
	case <-agent3.closeChan:
		t.Fatal("agent conn closed after we are healthy")
	case <-time.After(time.Second):
		// OK
	}
}

func TestPGCoordinator_Node_Empty(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	ctrl := gomock.NewController(t)
	mStore := dbmock.NewMockStore(ctrl)
	ps := pubsub.NewInMemory()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)

	id := uuid.New()
	mStore.EXPECT().GetTailnetPeers(gomock.Any(), id).Times(1).Return(nil, nil)

	// extra calls we don't particularly care about for this test
	mStore.EXPECT().UpsertTailnetCoordinator(gomock.Any(), gomock.Any()).
		AnyTimes().
		Return(database.TailnetCoordinator{}, nil)
	mStore.EXPECT().CleanTailnetCoordinators(gomock.Any()).AnyTimes().Return(nil)
	mStore.EXPECT().CleanTailnetLostPeers(gomock.Any()).AnyTimes().Return(nil)
	mStore.EXPECT().CleanTailnetTunnels(gomock.Any()).AnyTimes().Return(nil)
	mStore.EXPECT().UpdateTailnetPeerStatusByCoordinator(gomock.Any(), gomock.Any()).Times(1)

	uut, err := tailnet.NewPGCoord(ctx, logger, ps, mStore)
	require.NoError(t, err)
	defer func() {
		err := uut.Close()
		require.NoError(t, err)
	}()

	node := uut.Node(id)
	require.Nil(t, node)
}

// TestPGCoordinator_BidirectionalTunnels tests when peers create tunnels to each other.  We don't
// do this now, but it's schematically possible, so we should make sure it doesn't break anything.
func TestPGCoordinator_BidirectionalTunnels(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()
	agpltest.BidirectionalTunnels(ctx, t, coordinator)
}

func TestPGCoordinator_GracefulDisconnect(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()
	agpltest.GracefulDisconnectTest(ctx, t, coordinator)
}

func TestPGCoordinator_Lost(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()
	agpltest.LostTest(ctx, t, coordinator)
}

func TestPGCoordinator_NoDeleteOnClose(t *testing.T) {
	t.Parallel()
	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}
	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	coordinator, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator.Close()

	agent := newTestAgent(t, coordinator, "original")
	defer agent.close()
	agent.sendNode(&agpl.Node{PreferredDERP: 10})

	client := newTestClient(t, coordinator, agent.id)
	defer client.close()

	// Simulate some traffic to generate
	// a peer.
	agentNodes := client.recvNodes(ctx, t)
	require.Len(t, agentNodes, 1)
	assert.Equal(t, 10, agentNodes[0].PreferredDERP)
	client.sendNode(&agpl.Node{PreferredDERP: 11})

	clientNodes := agent.recvNodes(ctx, t)
	require.Len(t, clientNodes, 1)
	assert.Equal(t, 11, clientNodes[0].PreferredDERP)

	anode := coordinator.Node(agent.id)
	require.NotNil(t, anode)
	cnode := coordinator.Node(client.id)
	require.NotNil(t, cnode)

	err = coordinator.Close()
	require.NoError(t, err)
	assertEventuallyLost(ctx, t, store, agent.id)
	assertEventuallyLost(ctx, t, store, client.id)

	coordinator2, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	defer coordinator2.Close()

	anode = coordinator2.Node(agent.id)
	require.NotNil(t, anode)
	assert.Equal(t, 10, anode.PreferredDERP)

	cnode = coordinator2.Node(client.id)
	require.NotNil(t, cnode)
	assert.Equal(t, 11, cnode.PreferredDERP)
}

// TestPGCoordinatorDual_FailedHeartbeat tests that peers
// disconnect from a coordinator when they are unhealthy,
// are marked as LOST (not DISCONNECTED), and can reconnect to
// a new coordinator and reestablish their tunnels.
func TestPGCoordinatorDual_FailedHeartbeat(t *testing.T) {
	t.Parallel()

	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}

	dburl, closeFn, err := dbtestutil.Open()
	require.NoError(t, err)
	t.Cleanup(closeFn)

	store1, ps1, sdb1 := dbtestutil.NewDBWithSQLDB(t, dbtestutil.WithURL(dburl))
	defer sdb1.Close()
	store2, ps2, sdb2 := dbtestutil.NewDBWithSQLDB(t, dbtestutil.WithURL(dburl))
	defer sdb2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	t.Cleanup(cancel)

	// We do this to avoid failing due errors related to the
	// database connection being close.
	logger := slogtest.Make(t, &slogtest.Options{IgnoreErrors: true}).Leveled(slog.LevelDebug)

	// Create two coordinators, 1 for each peer.
	c1, err := tailnet.NewPGCoord(ctx, logger, ps1, store1)
	require.NoError(t, err)
	c2, err := tailnet.NewPGCoord(ctx, logger, ps2, store2)
	require.NoError(t, err)

	p1 := agpltest.NewPeer(ctx, t, c1, "peer1")
	p2 := agpltest.NewPeer(ctx, t, c2, "peer2")

	// Create a binding between the two.
	p1.AddTunnel(p2.ID)

	// Ensure that messages pass through.
	p1.UpdateDERP(1)
	p2.UpdateDERP(2)
	p1.AssertEventuallyHasDERP(p2.ID, 2)
	p2.AssertEventuallyHasDERP(p1.ID, 1)

	// Close the underlying database connection to induce
	// a heartbeat failure scenario and assert that
	// we eventually disconnect from the coordinator.
	err = sdb1.Close()
	require.NoError(t, err)
	p1.AssertEventuallyResponsesClosed()
	p2.AssertEventuallyLost(p1.ID)
	// This basically checks that peer2 had no update
	// performed on their status since we are connected
	// to coordinator2.
	assertEventuallyStatus(ctx, t, store2, p2.ID, database.TailnetStatusOk)

	// Connect peer1 to coordinator2.
	p1.ConnectToCoordinator(ctx, c2)
	// Reestablish binding.
	p1.AddTunnel(p2.ID)
	// Ensure messages still flow back and forth.
	p1.AssertEventuallyHasDERP(p2.ID, 2)
	p1.UpdateDERP(3)
	p2.UpdateDERP(4)
	p2.AssertEventuallyHasDERP(p1.ID, 3)
	p1.AssertEventuallyHasDERP(p2.ID, 4)
	// Make sure peer2 never got an update about peer1 disconnecting.
	p2.AssertNeverUpdateKind(p1.ID, proto.CoordinateResponse_PeerUpdate_DISCONNECTED)
}

func TestPGCoordinatorDual_PeerReconnect(t *testing.T) {
	t.Parallel()

	if !dbtestutil.WillUsePostgres() {
		t.Skip("test only with postgres")
	}

	store, ps := dbtestutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)

	// Create two coordinators, 1 for each peer.
	c1, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)
	c2, err := tailnet.NewPGCoord(ctx, logger, ps, store)
	require.NoError(t, err)

	p1 := agpltest.NewPeer(ctx, t, c1, "peer1")
	p2 := agpltest.NewPeer(ctx, t, c2, "peer2")

	// Create a binding between the two.
	p1.AddTunnel(p2.ID)

	// Ensure that messages pass through.
	p1.UpdateDERP(1)
	p2.UpdateDERP(2)
	p1.AssertEventuallyHasDERP(p2.ID, 2)
	p2.AssertEventuallyHasDERP(p1.ID, 1)

	// Close coordinator1. Now we will check that we
	// never send a DISCONNECTED update.
	err = c1.Close()
	require.NoError(t, err)
	p1.AssertEventuallyResponsesClosed()
	p2.AssertEventuallyLost(p1.ID)
	// This basically checks that peer2 had no update
	// performed on their status since we are connected
	// to coordinator2.
	assertEventuallyStatus(ctx, t, store, p2.ID, database.TailnetStatusOk)

	// Connect peer1 to coordinator2.
	p1.ConnectToCoordinator(ctx, c2)
	// Reestablish binding.
	p1.AddTunnel(p2.ID)
	// Ensure messages still flow back and forth.
	p1.AssertEventuallyHasDERP(p2.ID, 2)
	p1.UpdateDERP(3)
	p2.UpdateDERP(4)
	p2.AssertEventuallyHasDERP(p1.ID, 3)
	p1.AssertEventuallyHasDERP(p2.ID, 4)
	// Make sure peer2 never got an update about peer1 disconnecting.
	p2.AssertNeverUpdateKind(p1.ID, proto.CoordinateResponse_PeerUpdate_DISCONNECTED)
}

type testConn struct {
	ws, serverWS net.Conn
	nodeChan     chan []*agpl.Node
	sendNode     func(node *agpl.Node)
	errChan      <-chan error
	id           uuid.UUID
	closeChan    chan struct{}
}

func newTestConn(ids []uuid.UUID) *testConn {
	a := &testConn{}
	a.ws, a.serverWS = net.Pipe()
	a.nodeChan = make(chan []*agpl.Node)
	a.sendNode, a.errChan = agpl.ServeCoordinator(a.ws, func(nodes []*agpl.Node) error {
		a.nodeChan <- nodes
		return nil
	})
	if len(ids) > 1 {
		panic("too many")
	}
	if len(ids) == 1 {
		a.id = ids[0]
	} else {
		a.id = uuid.New()
	}
	a.closeChan = make(chan struct{})
	return a
}

func newTestAgent(t *testing.T, coord agpl.CoordinatorV1, name string, id ...uuid.UUID) *testConn {
	a := newTestConn(id)
	go func() {
		err := coord.ServeAgent(a.serverWS, a.id, name)
		assert.NoError(t, err)
		close(a.closeChan)
	}()
	return a
}

func newTestClient(t *testing.T, coord agpl.CoordinatorV1, agentID uuid.UUID, id ...uuid.UUID) *testConn {
	c := newTestConn(id)
	go func() {
		err := coord.ServeClient(c.serverWS, c.id, agentID)
		assert.NoError(t, err)
		close(c.closeChan)
	}()
	return c
}

func (c *testConn) close() error {
	return c.ws.Close()
}

func (c *testConn) recvNodes(ctx context.Context, t *testing.T) []*agpl.Node {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("testConn id %s: timeout receiving nodes ", c.id)
		return nil
	case nodes := <-c.nodeChan:
		return nodes
	}
}

func (c *testConn) recvErr(ctx context.Context, t *testing.T) error {
	t.Helper()
	// pgCoord works on eventual consistency, so it sometimes sends extra node
	// updates, and these block errors if not read from the nodes channel.
	for {
		select {
		case nodes := <-c.nodeChan:
			t.Logf("ignoring nodes update while waiting for error; id=%s, nodes=%+v",
				c.id.String(), nodes)
			continue
		case <-ctx.Done():
			t.Fatal("timeout receiving error")
			return ctx.Err()
		case err := <-c.errChan:
			return err
		}
	}
}

func (c *testConn) waitForClose(ctx context.Context, t *testing.T) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for connection to close")
		return
	case <-c.closeChan:
		return
	}
}

func assertEventuallyHasDERPs(ctx context.Context, t *testing.T, c *testConn, expected ...int) {
	t.Helper()
	for {
		nodes := c.recvNodes(ctx, t)
		if len(nodes) != len(expected) {
			t.Logf("expected %d, got %d nodes", len(expected), len(nodes))
			continue
		}

		derps := make([]int, 0, len(nodes))
		for _, n := range nodes {
			derps = append(derps, n.PreferredDERP)
		}
		for _, e := range expected {
			if !slices.Contains(derps, e) {
				t.Logf("expected DERP %d to be in %v", e, derps)
				continue
			}
			return
		}
	}
}

func assertNeverHasDERPs(ctx context.Context, t *testing.T, c *testConn, expected ...int) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			return
		case nodes := <-c.nodeChan:
			derps := make([]int, 0, len(nodes))
			for _, n := range nodes {
				derps = append(derps, n.PreferredDERP)
			}
			for _, e := range expected {
				if slices.Contains(derps, e) {
					t.Fatalf("expected not to get DERP %d, but received it", e)
					return
				}
			}
		}
	}
}

func assertEventuallyStatus(ctx context.Context, t *testing.T, store database.Store, agentID uuid.UUID, status database.TailnetStatus) {
	t.Helper()
	assert.Eventually(t, func() bool {
		peers, err := store.GetTailnetPeers(ctx, agentID)
		if xerrors.Is(err, sql.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, peer := range peers {
			if peer.Status != status {
				return false
			}
		}
		return true
	}, testutil.WaitShort, testutil.IntervalFast)
}

func assertEventuallyLost(ctx context.Context, t *testing.T, store database.Store, agentID uuid.UUID) {
	t.Helper()
	assertEventuallyStatus(ctx, t, store, agentID, database.TailnetStatusLost)
}

func assertEventuallyNoClientsForAgent(ctx context.Context, t *testing.T, store database.Store, agentID uuid.UUID) {
	t.Helper()
	assert.Eventually(t, func() bool {
		clients, err := store.GetTailnetTunnelPeerIDs(ctx, agentID)
		if xerrors.Is(err, sql.ErrNoRows) {
			return true
		}
		if err != nil {
			t.Fatal(err)
		}
		return len(clients) == 0
	}, testutil.WaitShort, testutil.IntervalFast)
}

type fakeCoordinator struct {
	ctx   context.Context
	t     *testing.T
	store database.Store
	id    uuid.UUID
}

func (c *fakeCoordinator) heartbeat() {
	c.t.Helper()
	_, err := c.store.UpsertTailnetCoordinator(c.ctx, c.id)
	require.NoError(c.t, err)
}

func (c *fakeCoordinator) agentNode(agentID uuid.UUID, node *agpl.Node) {
	c.t.Helper()
	pNode, err := agpl.NodeToProto(node)
	require.NoError(c.t, err)
	nodeRaw, err := gProto.Marshal(pNode)
	require.NoError(c.t, err)
	_, err = c.store.UpsertTailnetPeer(c.ctx, database.UpsertTailnetPeerParams{
		ID:            agentID,
		CoordinatorID: c.id,
		Node:          nodeRaw,
		Status:        database.TailnetStatusOk,
	})
	require.NoError(c.t, err)
}
