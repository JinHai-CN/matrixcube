package raftstore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/deepfabric/beehive/pb"
	"github.com/deepfabric/beehive/pb/bhmetapb"
	"github.com/deepfabric/beehive/pb/bhraftpb"
	"github.com/deepfabric/beehive/pb/raftcmdpb"
	"github.com/deepfabric/prophet/pb/metapb"
	"github.com/fagongzi/util/collection/deque"
	"github.com/fagongzi/util/protoc"
)

func (d *applyDelegate) execAdminRequest(ctx *applyContext) (*raftcmdpb.RaftCMDResponse, *execResult, error) {
	cmdType := ctx.req.AdminRequest.CmdType
	switch cmdType {
	case raftcmdpb.AdminCmdType_ChangePeer:
		return d.doExecChangePeer(ctx)
	case raftcmdpb.AdminCmdType_ChangePeerV2:
		return d.doExecChangePeerV2(ctx)
	case raftcmdpb.AdminCmdType_BatchSplit:
		return d.doExecSplit(ctx)
	case raftcmdpb.AdminCmdType_CompactLog:
		return d.doExecCompactRaftLog(ctx)
	}

	return nil, nil, nil
}

func (d *applyDelegate) doExecChangePeer(ctx *applyContext) (*raftcmdpb.RaftCMDResponse, *execResult, error) {
	req := ctx.req.AdminRequest.ChangePeer
	peer := req.Peer
	logger.Infof("shard %d do apply change peer %+v at epoch %+v",
		d.shard.ID,
		req,
		d.shard.Epoch)

	region := bhmetapb.Shard{}
	protoc.MustUnmarshal(&region, protoc.MustMarshal(&d.shard))
	region.Epoch.ConfVer++

	p := findPeer(&d.shard, req.Peer.ContainerID)
	switch req.ChangeType {
	case metapb.ChangePeerType_AddNode:
		exists := false
		if p != nil {
			exists = true
			if p.Role == metapb.PeerRole_Learner || p.ID != peer.ID {
				return nil, nil, fmt.Errorf("shard-%d can't add duplicated peer %+v",
					region.ID,
					peer)
			}

			p.Role = metapb.PeerRole_Voter
		}

		if !exists {
			region.Peers = append(region.Peers, peer)
		}

		logger.Infof("shard-%d add peer %+v successfully",
			region.ID,
			peer)
		break
	case metapb.ChangePeerType_RemoveNode:
		if p != nil {
			if *p != peer {
				return nil, nil, fmt.Errorf("shard %+v ignore remove unmatched peer %+v",
					region.ID,
					peer)
			}

			if d.peerID == peer.ID {
				// Remove ourself, we will destroy all shard data later.
				// So we need not to apply following logs.
				d.setPendingRemove()
			}
		} else {
			return nil, nil, fmt.Errorf("shard %+v remove missing peer %+v",
				region.ID,
				peer)
		}

		logger.Infof("shard-%d remove peer %+v successfully",
			region.ID,
			peer)
		break
	case metapb.ChangePeerType_AddLearnerNode:
		if p != nil {
			return nil, nil, fmt.Errorf("shard-%d can't add duplicated learner %+v",
				region.ID,
				peer)
		}

		region.Peers = append(region.Peers, peer)
		logger.Infof("shard-%d add learner peer %+v successfully",
			region.ID,
			peer)
		break
	}

	state := bhraftpb.PeerState_Normal
	if d.isPendingRemove() {
		state = bhraftpb.PeerState_Tombstone
	}

	d.shard = region
	err := d.ps.updatePeerState(d.shard, state, ctx.raftStateWB)
	if err != nil {
		logger.Fatalf("shard %d update db state failed, errors:\n %+v",
			d.shard.ID,
			err)
	}

	resp := newAdminRaftCMDResponse(raftcmdpb.AdminCmdType_ChangePeer, &raftcmdpb.ChangePeerResponse{
		Shard: d.shard,
	})
	result := &execResult{
		adminType: raftcmdpb.AdminCmdType_ChangePeer,
		changePeer: &changePeer{
			index:   d.ctx.index,
			changes: []raftcmdpb.ChangePeerRequest{*req},
			shard:   d.shard,
		},
	}

	return resp, result, nil
}

func (d *applyDelegate) doExecChangePeerV2(ctx *applyContext) (*raftcmdpb.RaftCMDResponse, *execResult, error) {
	req := ctx.req.AdminRequest.ChangePeerV2
	changes := req.Changes
	logger.Infof("shard %d do apply change peer v2 %+v at epoch %+v",
		d.shard.ID,
		changes,
		d.shard.Epoch)

	var region bhmetapb.Shard
	var err error
	kind := getConfChangeKind(len(changes))
	if kind == leaveJointKind {
		region, err = d.applyLeaveJoint()
	} else {
		region, err = d.applyConfChangeByKind(kind, changes)
	}

	if err != nil {
		return nil, nil, err
	}

	state := bhraftpb.PeerState_Normal
	if d.isPendingRemove() {
		state = bhraftpb.PeerState_Tombstone
	}

	d.shard = region
	err = d.ps.updatePeerState(d.shard, state, ctx.raftStateWB)
	if err != nil {
		logger.Fatalf("shard %d update db state failed, errors:\n %+v",
			d.shard.ID,
			err)
	}

	resp := newAdminRaftCMDResponse(raftcmdpb.AdminCmdType_ChangePeer, &raftcmdpb.ChangePeerResponse{
		Shard: d.shard,
	})
	result := &execResult{
		adminType: raftcmdpb.AdminCmdType_ChangePeer,
		changePeer: &changePeer{
			index:   d.ctx.index,
			changes: changes,
			shard:   d.shard,
		},
	}

	return resp, result, nil
}

func (d *applyDelegate) applyConfChangeByKind(kind confChangeKind, changes []raftcmdpb.ChangePeerRequest) (bhmetapb.Shard, error) {
	region := bhmetapb.Shard{}
	protoc.MustUnmarshal(&region, protoc.MustMarshal(&d.shard))

	for _, cp := range changes {
		change_type := cp.ChangeType
		peer := cp.Peer
		store_id := peer.ContainerID

		exist_peer := findPeer(&d.shard, peer.ContainerID)
		if exist_peer != nil {
			r := exist_peer.Role
			if r == metapb.PeerRole_IncomingVoter || r == metapb.PeerRole_DemotingVoter {
				logger.Fatalf("shard-%d can't apply confchange because configuration is still in joint state",
					region.ID)
			}
		}

		if exist_peer == nil && change_type == metapb.ChangePeerType_AddNode {
			if kind == simpleKind {
				peer.Role = metapb.PeerRole_Voter
			} else if kind == enterJointKind {
				peer.Role = metapb.PeerRole_IncomingVoter
			}

			region.Peers = append(region.Peers, peer)
		} else if exist_peer == nil && change_type == metapb.ChangePeerType_AddLearnerNode {
			peer.Role = metapb.PeerRole_Learner
			region.Peers = append(region.Peers, peer)
		} else if exist_peer == nil && change_type == metapb.ChangePeerType_RemoveNode {
			return region, fmt.Errorf("remove missing peer %+v", peer)
		} else if exist_peer != nil &&
			(change_type == metapb.ChangePeerType_AddNode || change_type == metapb.ChangePeerType_AddLearnerNode) {
			// add node
			role := exist_peer.Role
			exist_id := exist_peer.ID
			incoming_id := peer.ID

			// Add peer with different id to the same store
			if exist_id != incoming_id ||
				// The peer is already the requested role
				(role == metapb.PeerRole_Voter && change_type == metapb.ChangePeerType_AddNode) ||
				(role == metapb.PeerRole_Learner && change_type == metapb.ChangePeerType_AddLearnerNode) {
				return region, fmt.Errorf("can't add duplicated peer %+v, duplicated with exist peer %+v",
					peer, exist_peer)
			}

			if role == metapb.PeerRole_Voter && change_type == metapb.ChangePeerType_AddLearnerNode {
				switch kind {
				case simpleKind:
					exist_peer.Role = metapb.PeerRole_Learner
				case enterJointKind:
					exist_peer.Role = metapb.PeerRole_DemotingVoter
				}
			} else if role == metapb.PeerRole_Learner && change_type == metapb.ChangePeerType_AddNode {
				switch kind {
				case simpleKind:
					exist_peer.Role = metapb.PeerRole_Voter
				case enterJointKind:
					exist_peer.Role = metapb.PeerRole_IncomingVoter
				}
			}
		} else if exist_peer != nil && change_type == metapb.ChangePeerType_RemoveNode {
			// Remove node
			if kind == enterJointKind && exist_peer.Role == metapb.PeerRole_Voter {
				return region, fmt.Errorf("can't remove voter peer %+v directly",
					peer)
			}

			p := removePeer(&region, store_id)
			if p != nil {
				if *p != peer {
					return region, fmt.Errorf("ignore remove unmatched peer %+v", peer)
				}

				if d.peerID == peer.ID {
					// Remove ourself, we will destroy all region data later.
					// So we need not to apply following logs.
					d.setPendingRemove()
				}
			}
		}
	}

	region.Epoch.ConfVer += uint64(len(changes))
	logger.Infof("shard-%d conf change successfully, changes %+v",
		region.ID,
		changes)
	return region, nil
}

func (d *applyDelegate) applyLeaveJoint() (bhmetapb.Shard, error) {
	region := bhmetapb.Shard{}
	protoc.MustUnmarshal(&region, protoc.MustMarshal(&d.shard))

	change_num := uint64(0)
	for idx := range region.Peers {
		if region.Peers[idx].Role == metapb.PeerRole_IncomingVoter {
			region.Peers[idx].Role = metapb.PeerRole_Voter
			continue
		}

		if region.Peers[idx].Role == metapb.PeerRole_DemotingVoter {
			region.Peers[idx].Role = metapb.PeerRole_Learner
			continue
		}

		change_num += 1
	}
	if change_num == 0 {
		logger.Fatalf("shard-%d can't leave a non-joint config %+v",
			d.shard.ID,
			region)
	}
	region.Epoch.ConfVer += change_num
	logger.Infof(
		"shard-%d leave joint state successfully", d.shard.ID)
	return region, nil
}

func (d *applyDelegate) doExecSplit(ctx *applyContext) (*raftcmdpb.RaftCMDResponse, *execResult, error) {
	ctx.metrics.admin.split++
	splitReqs := ctx.req.AdminRequest.Splits

	if len(splitReqs.Requests) == 0 {
		logger.Errorf("shard %d missing splits request",
			d.shard.ID)
		return nil, nil, errors.New("missing splits request")
	}

	new_region_cnt := len(splitReqs.Requests)

	derived := bhmetapb.Shard{}
	protoc.MustUnmarshal(&derived, protoc.MustMarshal(&d.shard))
	var regions []bhmetapb.Shard
	keys := deque.New()

	for _, req := range splitReqs.Requests {
		if len(req.SplitKey) == 0 {
			return nil, nil, errors.New("missing split key")
		}

		split_key := DecodeDataKey(req.SplitKey)
		v := derived.Start
		if e, ok := keys.Back(); ok {
			v = e.Value.([]byte)
		}
		if bytes.Compare(split_key, v) <= 0 {
			return nil, nil, fmt.Errorf("invalid split key %+v", split_key)
		}

		if len(req.NewPeerIDs) != len(derived.Peers) {
			return nil, nil, fmt.Errorf("invalid new peer id count, need %d, but got %d",
				len(derived.Peers),
				len(req.NewPeerIDs))
		}

		keys.PushBack(split_key)
	}

	v, _ := keys.Back()
	err := checkKeyInShard(v.Value.([]byte), &d.shard)
	if err != nil {
		logger.Errorf("shard %d split key not in shard, errors:\n %+v",
			d.shard.ID,
			err)
		return nil, nil, nil
	}

	derived.Epoch.Version += uint64(new_region_cnt)
	keys.PushBack(derived.End)
	v, _ = keys.Front()
	derived.End = v.Value.([]byte)
	regions = append(regions, newResourceAdapterWithShard(derived).Clone().(resourceAdapter).meta)

	// req.SplitKey = DecodeDataKey(req.SplitKey)

	// // splitKey < shard.Startkey
	// if bytes.Compare(req.SplitKey, d.shard.Start) < 0 {
	// 	logger.Errorf("shard %d invalid split key, split=<%+v> shard-start=<%+v>",
	// 		d.shard.ID,
	// 		req.SplitKey,
	// 		d.shard.Start)
	// 	return nil, nil, nil
	// }

	// peer := checkKeyInShard(req.SplitKey, &d.shard)
	// if peer != nil {
	// 	logger.Errorf("shard %d split key not in shard, errors:\n %+v",
	// 		d.shard.ID,
	// 		peer)
	// 	return nil, nil, nil
	// }

	// if len(req.NewPeerIDs) != len(d.shard.Peers) {
	// 	logger.Errorf("shard %d invalid new peer id count, splitCount=<%d> currentCount=<%d>",
	// 		d.shard.ID,
	// 		len(req.NewPeerIDs),
	// 		len(d.shard.Peers))

	// 	return nil, nil, nil
	// }

	// logger.Infof("shard %d split, splitKey=<%d> shard=<%+v>",
	// 	d.shard.ID,
	// 	req.SplitKey,
	// 	d.shard)

	// // After split, the origin shard key range is [start_key, split_key),
	// // the new split shard is [split_key, end).
	// newShard := bhmetapb.Shard{
	// 	ID:    req.NewShardID,
	// 	Epoch: d.shard.Epoch,
	// 	Start: req.SplitKey,
	// 	End:   d.shard.End,
	// 	Group: d.shard.Group,
	// }
	// d.shard.End = req.SplitKey

	// for idx, id := range req.NewPeerIDs {
	// 	newShard.Peers = append(newShard.Peers, metapb.Peer{
	// 		ID:      id,
	// 		StoreID: d.shard.Peers[idx].ContainerID,
	// 	})
	// }

	// d.shard.Epoch.Version++
	// newShard.Epoch.Version = d.shard.Epoch.Version

	// if d.store.cfg.Customize.CustomSplitCompletedFunc != nil {
	// 	d.store.cfg.Customize.CustomSplitCompletedFunc(&d.shard, &newShard)
	// }

	// err := d.ps.updatePeerState(d.shard, bhraftpb.PeerState_Normal, ctx.raftStateWB)

	// d.wb.Reset()
	// if err == nil {
	// 	err = d.ps.updatePeerState(newShard, bhraftpb.PeerState_Normal, d.wb)
	// }

	// if err == nil {
	// 	err = d.ps.writeInitialState(newShard.ID, d.wb)
	// }
	// if err != nil {
	// 	logger.Fatalf("shard %d save split shard failed, newShard=<%+v> errors:\n %+v",
	// 		d.shard.ID,
	// 		newShard,
	// 		err)
	// }

	// err = d.ps.store.MetadataStorage().Write(d.wb, false)
	// if err != nil {
	// 	logger.Fatalf("shard %d commit apply result failed, errors:\n %+v",
	// 		d.shard.ID,
	// 		err)
	// }

	// rsp := newAdminRaftCMDResponse(raftcmdpb.AdminCmdType_BatchSplit, &raftcmdpb.SplitResponse{
	// 	Left:  d.shard,
	// 	Right: newShard,
	// })

	// result := &execResult{
	// 	adminType: raftcmdpb.AdminCmdType_BatchSplit,
	// 	splitResult: &splitResult{
	// 		left:  d.shard,
	// 		right: newShard,
	// 	},
	// }

	// ctx.metrics.admin.splitSucceed++
	// return rsp, result, nil
}

func (d *applyDelegate) doExecCompactRaftLog(ctx *applyContext) (*raftcmdpb.RaftCMDResponse, *execResult, error) {
	ctx.metrics.admin.compact++

	req := ctx.req.AdminRequest.CompactLog
	compactIndex := req.CompactIndex
	firstIndex := ctx.applyState.TruncatedState.Index + 1

	if compactIndex <= firstIndex {
		return nil, nil, nil
	}

	compactTerm := req.CompactTerm
	if compactTerm == 0 {
		return nil, nil, errors.New("command format is outdated, please upgrade leader")
	}

	err := compactRaftLog(d.shard.ID, &ctx.applyState, compactIndex, compactTerm)
	if err != nil {
		return nil, nil, err
	}

	rsp := newAdminRaftCMDResponse(raftcmdpb.AdminCmdType_CompactLog, &raftcmdpb.CompactLogRequest{})
	result := &execResult{
		adminType: raftcmdpb.AdminCmdType_CompactLog,
		raftGCResult: &raftGCResult{
			state:      ctx.applyState.TruncatedState,
			firstIndex: firstIndex,
		},
	}

	ctx.metrics.admin.compactSucceed++
	return rsp, result, nil
}

func (d *applyDelegate) execWriteRequest(ctx *applyContext) (uint64, int64, *raftcmdpb.RaftCMDResponse) {
	writeBytes := uint64(0)
	diffBytes := int64(0)
	resp := pb.AcquireRaftCMDResponse()

	d.resetAttrs()
	d.buf.Clear()
	d.requests = d.requests[:0]

	for idx, req := range ctx.req.Requests {
		if logger.DebugEnabled() {
			logger.Debugf("%s exec", hex.EncodeToString(req.ID))
		}
		resp.Responses = append(resp.Responses, nil)

		ctx.metrics.writtenKeys++
		if ctx.dataWB != nil {
			addedToWB, rsp, err := ctx.dataWB.Add(d.shard.ID, req, d.attrs)
			if err != nil {
				logger.Fatalf("shard %s add %+v to write batch failed with %+v",
					d.shard.ID,
					req,
					err)
			}

			if addedToWB {
				resp.Responses[idx] = rsp
				continue
			}
		}

		d.requests = append(d.requests, idx)
	}

	if len(d.requests) > 0 {
		d.attrs[attrRequestsTotal] = len(d.requests) - 1
		for idx, which := range d.requests {
			req := ctx.req.Requests[which]
			d.attrs[attrRequestsCurrent] = idx
			if h, ok := d.store.writeHandlers[req.CustemType]; ok {
				written, diff, rsp := h(d.shard, req, d.attrs)
				if rsp.Stale {
					rsp.Error.Message = errStaleCMD.Error()
					rsp.Error.StaleCommand = infoStaleCMD
					rsp.OriginRequest = req
					rsp.OriginRequest.Key = DecodeDataKey(req.Key)
				}

				resp.Responses[which] = rsp
				writeBytes += written
				diffBytes += diff
			}
		}
	}

	return writeBytes, diffBytes, resp
}
