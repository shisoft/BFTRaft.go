package client

import (
	"context"
	"crypto/rsa"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	spb "github.com/PomeloCloud/BFTRaft4go/proto/server"
	"github.com/PomeloCloud/BFTRaft4go/utils"
	"github.com/patrickmn/go-cache"
	"log"
)

type BFTRaftClient struct {
	Id          uint64
	PrivateKey  *rsa.PrivateKey
	AlphaRPCs   AlphaRPCsCache
	GroupHosts  *cache.Cache
	GroupLeader *cache.Cache
	CmdResChan  map[uint64]map[uint64]chan []byte
	Counter     int64
	Lock        sync.RWMutex
}

type ClientOptions struct {
	PrivateKey []byte
}

// bootstraps is a list of server address believed to be the member of the network
// the list does not need to contain alpha nodes since all of the nodes on the network will get informed
func NewClient(bootstraps []string, opts ClientOptions) (*BFTRaftClient, error) {
	privateKey, err := utils.ParsePrivateKey(opts.PrivateKey)
	if err != nil {
		return nil, err
	}
	publicKey := utils.PublicKeyFromPrivate(privateKey)
	bftclient := &BFTRaftClient{
		Id:          utils.HashPublicKey(publicKey),
		PrivateKey:  privateKey,
		Lock:        sync.RWMutex{},
		AlphaRPCs:   NewAlphaRPCsCache(bootstraps),
		GroupHosts:  cache.New(1*time.Minute, 1*time.Minute),
		GroupLeader: cache.New(1*time.Minute, 1*time.Minute),
		CmdResChan:  map[uint64]map[uint64]chan []byte{},
		Counter:     0,
	}
	return bftclient, nil
}

func (brc *BFTRaftClient) GetGroupHosts(groupId uint64) *[]*spb.Host {
	cacheKey := strconv.Itoa(int(groupId))
	if cached, found := brc.GroupHosts.Get(cacheKey); found {
		return cached.(*[]*spb.Host)
	}
	res := utils.MajorityResponse(brc.AlphaRPCs.Get(), func(client spb.BFTRaftClient) (interface{}, []byte) {
		if res, err := client.GroupHosts(
			context.Background(), &spb.GroupId{GroupId: groupId},
		); err == nil {
			return &res.Nodes, utils.NodesSignData(res.Nodes)
		} else {
			log.Println("error on getting group host:", err)
			return nil, []byte{}
		}
	})
	var hosts *[]*spb.Host = nil
	if res != nil {
		hosts = res.(*[]*spb.Host)
	}
	if hosts != nil {
		brc.GroupHosts.Set(cacheKey, hosts, cache.DefaultExpiration)
	}
	return hosts
}

func (brc *BFTRaftClient) GetGroupLeader(groupId uint64) spb.BFTRaftClient {
	cacheKey := strconv.Itoa(int(groupId))
	if cached, found := brc.GroupLeader.Get(cacheKey); found {
		return cached.(spb.BFTRaftClient)
	}
	res := utils.MajorityResponse(brc.AlphaRPCs.Get(), func(client spb.BFTRaftClient) (interface{}, []byte) {
		if res, err := client.GetGroupLeader(
			context.Background(), &spb.GroupId{GroupId: groupId},
		); err == nil {
			// TODO: verify signature
			if res.Node == nil {
				log.Println("nil response for get leader for group:", groupId)
			}
			return res.Node, []byte(res.Node.ServerAddr)
		} else {
			log.Println("cannot get group leader on alpha peer:", err)
			return nil, []byte{}
		}
	})
	var leaderHost *spb.Host = nil
	if res != nil {
		leaderHost = res.(*spb.Host)
	}
	if leaderHost != nil {
		if leader, err := utils.GetClusterRPC(leaderHost.ServerAddr); err == nil {
			brc.GroupLeader.Set(cacheKey, leader, cache.DefaultExpiration)
			return leader
		}
	} else {
		log.Println(brc.Id, ", group", groupId, "has no leader")
	}
	return nil
}

func (brc *BFTRaftClient) GroupExists(groupId uint64) bool {
	res := utils.MajorityResponse(brc.AlphaRPCs.Get(), func(client spb.BFTRaftClient) (interface{}, []byte) {
		if _, err := client.GetGroupContent(
			context.Background(), &spb.GroupId{GroupId: groupId},
		); err == nil {
			// TODO: verify signature
			return true, []byte{1}
		} else {
			return false, []byte{0}
		}
	})
	return res.(bool)
}

func (brc *BFTRaftClient) ExecCommand(groupId uint64, funcId uint64, arg []byte) (*[]byte, error) {
	leader := brc.GetGroupLeader(groupId)
	if leader == nil {
		return nil, errors.New("cannot found leader")
	}
	reqId := uint64(atomic.AddInt64(&brc.Counter, 1))
	cmdReq := &spb.CommandRequest{
		Group:     groupId,
		ClientId:  brc.Id,
		RequestId: reqId,
		FuncId:    funcId,
		Arg:       arg,
	}
	signData := utils.ExecCommandSignData(cmdReq)
	cmdReq.Signature = utils.Sign(brc.PrivateKey, signData)
	if _, found := brc.CmdResChan[groupId]; !found {
		brc.CmdResChan[groupId] = map[uint64]chan []byte{}
	}
	brc.CmdResChan[groupId][reqId] = make(chan []byte)
	defer func() {
		close(brc.CmdResChan[groupId][reqId])
		delete(brc.CmdResChan[groupId], reqId)
	}()
	go func() {
		if cmdRes, err := leader.ExecCommand(context.Background(), cmdReq); err == nil {
			// TODO: verify signature
			// TODO: update leader if needed
			// TODO: verify response matches request
			brc.CmdResChan[groupId][reqId] <- cmdRes.Result

		} else {
			log.Println("cannot exec on leader:", err)
		}
	}()
	hosts := brc.GetGroupHosts(groupId)
	if hosts == nil {
		return nil, errors.New("cannot get group hosts")
	}
	expectedResponse := utils.ExpectedPlayers(len(*hosts))
	responseReceived := map[uint64][]byte{}
	responseHashes := []uint64{}
	replicationCompleted := make(chan bool, 1)
	wg := sync.WaitGroup{}
	wg.Add(expectedResponse)
	go func() {
		for i := 0; i < expectedResponse; i++ {
			res := <-brc.CmdResChan[groupId][reqId]
			hash := utils.HashData(res)
			responseReceived[hash] = res
			responseHashes = append(responseHashes, hash)
			wg.Done()
		}
	}()
	go func() {
		wg.Wait()
		replicationCompleted <- true
	}()
	select {
	case <-replicationCompleted:
		majorityHash := utils.PickMajority(responseHashes)
		majorityData := responseReceived[majorityHash]
		return &majorityData, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("does not receive enough response")
	}
}
