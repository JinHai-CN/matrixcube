package statistics

import (
	"math/rand"
	"testing"

	"github.com/matrixorigin/matrixcube/components/prophet/core"
	"github.com/matrixorigin/matrixcube/components/prophet/metadata"
	"github.com/matrixorigin/matrixcube/components/prophet/pb/metapb"
	"github.com/stretchr/testify/assert"
)

func TestContainerTimeUnsync(t *testing.T) {
	cache := newHotContainersStats(WriteFlow)
	peers := newPeers(3,
		func(i int) uint64 { return uint64(10000 + i) },
		func(i int) uint64 { return uint64(i) })
	meta := &metadata.TestResource{
		ResID:    1000,
		ResPeers: peers,
		Start:    []byte(""),
		End:      []byte(""),
		ResEpoch: metapb.ResourceEpoch{ConfVer: 6, Version: 6},
	}
	intervals := []uint64{120, 60}
	for _, interval := range intervals {
		resource := core.NewCachedResource(meta, &peers[0],
			// interval is [0, interval]
			core.SetReportInterval(interval),
			core.SetWrittenBytes(interval*100*1024))

		checkAndUpdate(t, cache, resource, 3)
		{
			stats := cache.ResourceStats()
			assert.Equal(t, 3, len(stats))
			for _, s := range stats {
				assert.Equal(t, 1, len(s))
			}
		}
	}
}

type operator int

const (
	transferLeader operator = iota
	movePeer
	addReplica
)

type testCacheCase struct {
	kind     FlowKind
	operator operator
	expect   int
}

func TestCache(t *testing.T) {
	tests := []*testCacheCase{
		{ReadFlow, transferLeader, 2},
		{ReadFlow, movePeer, 1},
		{ReadFlow, addReplica, 1},
		{WriteFlow, transferLeader, 3},
		{WriteFlow, movePeer, 4},
		{WriteFlow, addReplica, 4},
	}
	for _, c := range tests {
		testCache(t, c)
	}
}

func testCache(t *testing.T, c *testCacheCase) {
	defaultSize := map[FlowKind]int{
		ReadFlow:  1, // only leader
		WriteFlow: 3, // all peers
	}
	cache := newHotContainersStats(c.kind)
	resource := buildresource(nil, nil, c.kind)
	checkAndUpdate(t, cache, resource, defaultSize[c.kind])
	checkHit(t, cache, resource, c.kind, false) // all peers are new

	srcContainer, resource := schedule(c.operator, resource, c.kind)
	res := checkAndUpdate(t, cache, resource, c.expect)
	checkHit(t, cache, resource, c.kind, true) // hit cache
	if c.expect != defaultSize[c.kind] {
		checkNeedDelete(t, res, srcContainer)
	}
}

func checkAndUpdate(t *testing.T, cache *hotPeerCache, resource *core.CachedResource, expect int) []*HotPeerStat {
	res := cache.CheckResourceFlow(resource)
	assert.Equal(t, expect, len(res))
	for _, p := range res {
		cache.Update(p)
	}
	return res
}

func checkHit(t *testing.T, cache *hotPeerCache, resource *core.CachedResource, kind FlowKind, isHit bool) {
	var peers []metapb.Peer
	if kind == ReadFlow {
		peers = []metapb.Peer{*resource.GetLeader()}
	} else {
		peers = resource.Meta.Peers()
	}
	for _, peer := range peers {
		item := cache.getOldHotPeerStat(resource.Meta.ID(), peer.ContainerID)
		assert.NotNil(t, item)
		assert.Equal(t, !isHit, item.isNew)
	}
}

func checkNeedDelete(t *testing.T, ret []*HotPeerStat, ContainerID uint64) {
	for _, item := range ret {
		if item.ContainerID == ContainerID {
			assert.True(t, item.needDelete)
			return
		}
	}
}

func schedule(operator operator, resource *core.CachedResource, kind FlowKind) (srcContainer uint64, _ *core.CachedResource) {
	switch operator {
	case transferLeader:
		_, newLeader := pickFollower(resource)
		return resource.GetLeader().ContainerID, buildresource(resource.Meta, &newLeader, kind)
	case movePeer:
		index, _ := pickFollower(resource)
		meta := resource.Meta.(*metadata.TestResource)
		srcContainer := meta.ResPeers[index].ContainerID
		meta.ResPeers[index] = metapb.Peer{ID: 4, ContainerID: 4}
		return srcContainer, buildresource(meta, resource.GetLeader(), kind)
	case addReplica:
		meta := resource.Meta.(*metadata.TestResource)
		meta.ResPeers = append(meta.ResPeers, metapb.Peer{ID: 4, ContainerID: 4})
		return 0, buildresource(meta, resource.GetLeader(), kind)
	default:
		return 0, nil
	}
}

func pickFollower(resource *core.CachedResource) (index int, peer metapb.Peer) {
	var dst int
	meta := resource.Meta.(*metadata.TestResource)

	for index, peer := range meta.ResPeers {
		if peer.ContainerID == resource.GetLeader().ContainerID {
			continue
		}
		dst = index
		if rand.Intn(2) == 0 {
			break
		}
	}
	return dst, meta.ResPeers[dst]
}

func buildresource(meta metadata.Resource, leader *metapb.Peer, kind FlowKind) *core.CachedResource {
	const interval = uint64(60)
	if meta == nil {
		peer1 := metapb.Peer{ID: 1, ContainerID: 1}
		peer2 := metapb.Peer{ID: 2, ContainerID: 2}
		peer3 := metapb.Peer{ID: 3, ContainerID: 3}

		meta = &metadata.TestResource{
			ResID:    1000,
			ResPeers: []metapb.Peer{peer1, peer2, peer3},
			Start:    []byte(""),
			End:      []byte(""),
			ResEpoch: metapb.ResourceEpoch{ConfVer: 6, Version: 6},
		}
		leader = &meta.Peers()[rand.Intn(3)]
	}

	switch kind {
	case ReadFlow:
		return core.NewCachedResource(meta, leader, core.SetReportInterval(interval),
			core.SetReadBytes(interval*100*1024))
	case WriteFlow:
		return core.NewCachedResource(meta, leader, core.SetReportInterval(interval),
			core.SetWrittenBytes(interval*100*1024))
	default:
		return nil
	}
}

type genID func(i int) uint64

func newPeers(n int, pid genID, sid genID) []metapb.Peer {
	peers := make([]metapb.Peer, 0, n)
	for i := 1; i <= n; i++ {
		peer := metapb.Peer{
			ID: pid(i),
		}
		peer.ContainerID = sid(i)
		peers = append(peers, peer)
	}
	return peers
}
