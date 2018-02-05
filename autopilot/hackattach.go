package autopilot

import (
	"fmt"
	prand "math/rand"
	"time"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

// HackyPrefAttachment is an implementation of the AttachmentHeuristic
// interface that implements my own idea of who and how to automatically connect which I may or may not care to elaborate further upon here
//
// TODO: elaborate on the above.

type HackyPrefAttachment struct {
	minChanSize btcutil.Amount
	maxChanSize btcutil.Amount

	chanLimit uint16

	threshold float64
}

// NewHackyPrefAttachment creates a new instance of a
// HackyPrefAttachment heuristics given bounds on allowed channel sizes,
// and an allocation amount which is interpreted as a percentage of funds that
// is to be committed to channels at all times.
func NewHackyPrefAttachment(minChanSize, maxChanSize btcutil.Amount,
	chanLimit uint16, allocation float64) *HackyPrefAttachment {

	prand.Seed(time.Now().Unix())

	return &HackyPrefAttachment{
		minChanSize: minChanSize,
		chanLimit:   chanLimit,
		maxChanSize: maxChanSize,
		threshold:   allocation,
	}
}

// A compile time assertion to ensure HackyPrefAttachment meets the
// AttachmentHeuristic interface.
var _ AttachmentHeuristic = (*HackyPrefAttachment)(nil)

// NeedMoreChans is a predicate that should return true if, given the passed
// parameters, and its internal state, more channels should be opened within
// the channel graph. If the heuristic decides that we do indeed need more
// channels, then the second argument returned will represent the amount of
// additional funds to be used towards creating channels.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
// BMC todo: dont count inbound channels.
func (p *HackyPrefAttachment) NeedMoreChans(channels []Channel,
	funds btcutil.Amount) (btcutil.Amount, bool) {

	// If we're already over our maximum allowed number of channels, then
	// we'll instruct the controller not to create any more channels.
	// log.Debugf("NeedMoreChans: num chans: %d, chan limit: %d", len(channels), int(p.chanLimit))
	// if len(channels) >= int(p.chanLimit) {
	// 	return 0, false
	// }

	// First, we'll tally up the total amount of funds that are currently
	// present within the set of active channels.
	var totalChanAllocation btcutil.Amount
	ourChannelsCount := 0
	for _, channel := range channels {
		if !channel.IsInitiator {
			continue
		}
		ourChannelsCount += 1
		totalChanAllocation += channel.Capacity
	}
	log.Warnf("NeedMoreChans: num chans: %d, chan limit: %d", ourChannelsCount, int(p.chanLimit))
	if ourChannelsCount >= int(p.chanLimit) {
		return 0, false
	}

	// With this value known, we'll now compute the total amount of fund
	// allocated across regular utxo's and channel utxo's.
	totalFunds := funds + totalChanAllocation

	// Once the total amount has been computed, we then calculate the
	// fraction of funds currently allocated to channels.
	fundsFraction := float64(totalChanAllocation) / float64(totalFunds)

	// If this fraction is below our threshold, then we'll return true, to
	// indicate the controller should call Select to obtain a candidate set
	// of channels to attempt to open.
	needMore := fundsFraction < p.threshold
	log.Warnf("NeedMoreChans: totalChanAllocation: %v, funds: %v, totalFunds: %v, fundsFraction %.2f, needMore: %t", totalChanAllocation, funds, totalFunds, fundsFraction, needMore)
	if !needMore {
		return 0, false
	}

	// Now that we know we need more funds, we'll compute the amount of
	// additional funds we should allocate towards channels.
	targetAllocation := btcutil.Amount(float64(totalFunds) * p.threshold)
	fundsAvailable := targetAllocation - totalChanAllocation
	log.Warnf("NeedMoreChans: targetAllocation: %v, fundsAvailable: %v", targetAllocation, fundsAvailable)
	return fundsAvailable, true
}

// Select returns a candidate set of attachment directives that should be
// executed based on the current internal state, the state of the channel
// graph, the set of nodes we should exclude, and the amount of funds
// available. The heuristic employed by this method is one that attempts to
// promote a scale-free network globally, via local attachment preferences for
// new nodes joining the network with an amount of available funds to be
// allocated to channels. Specifically, we consider the degree of each node
// (and the flow in/out of the node available via its open channels) and
// utilize the Barabási–Albert model to drive our recommended attachment
// heuristics. If implemented globally for each new participant, this results
// in a channel graph that is scale-free and follows a power law distribution
// with k=-3.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (p *HackyPrefAttachment) Select(self *btcec.PublicKey, g ChannelGraph,
	fundsAvailable btcutil.Amount,
	skipNodes map[NodeID]struct{}, totalChans []Channel) ([]AttachmentDirective, error) {

	// TODO(roasbeef): rename?

	var directives []AttachmentDirective

	if fundsAvailable < p.minChanSize {
		return directives, nil
	}
	numChansToOpen := int(p.chanLimit) - len(totalChans)
	// log.Warnf("Select: want to have total of %d channels, need to open %d to achieve that. Will select from %d unique (%d weighted) node choices, have %d channels and %d skipNodes out of the gate!!.", p.chanLimit, numChansToOpen, len(idToNode), len(selectionSlice), len(totalChans), len(skipNodes))
	log.Warnf("Select: want to have total of %d channels, need to open %d to achieve that. (already have %d chans open)", p.chanLimit, numChansToOpen, len(totalChans))
	if numChansToOpen <= 0 {
		return directives, nil
	}

	// selectionSlice will be used to randomly select a node
	// according to a power law distribution. For each connected
	// edge, we'll add an instance of the node to this slice. Thus,
	// for a given node, the probability that we'll attach to it
	// is: k_i / sum(k_j), where k_i is the degree of the target
	// node, and k_j is the degree of all other nodes i != j. This
	// implements the classic Barabási–Albert model for
	// preferential attachment.
	//
	// BMC change: selected randomly by weight. existing should give some amount of weight, say a standard max channel allocation.
	//             the value in channels should be added as weight to both nodes involved, unless one of those nodes is us.

	var selectionSlice []NodeID
	var idToNode = make(map[NodeID]Node)
	// var nodeWeightsByNodeID = make(map[NodeID]btcutil.Amount)

	// For each node, and each channel that the node has, we'll add
	// an instance of that node to the selection slice above.
	// This'll slice where the frequency of each node is equivalent
	// to the number of channels that connect to it.
	//
	// TODO(roasbeef): add noise to make adversarially resistant?
	log.Warn("Select: calling g.ForEachNode")
	if err := g.ForEachNode(func(node Node) error {
		nID := NewNodeID(node.PubKey())
		// If we come across ourselves, them we'll continue in
		// order to avoid attempting to make a channel with
		// ourselves.
		if node.PubKey().IsEqual(self) {
			return nil
		}

		// Additionally, if this node is in the blacklist, then
		// we'll skip it.
		if _, ok := skipNodes[nID]; ok {
			return nil
		}

		idToNode[nID] = node

		// For initial bootstrap purposes, if a node doesn't
		// have any channels, then we'll ensure that it has at
		// least one item in the selection slice.
		//
		// TODO(roasbeef): make conditional?
		selectionSlice = append(selectionSlice, nID)
		// nodeWeightsByNodeID[nID] += p.maxChanSize //basic weight for existing. the bigger channels we plan to open, i guess the more important randos existence is?

		// For each active channel the node has, we'll add an
		// additional channel to the selection slice to
		// increase their weight.
		if err := node.ForEachChannel(func(channel ChannelEdge) error {

			//as we are exploring the channels of a node here, the peer is the other node that this channel connects to. (?? confirm, 99% sure this has to be true here. ??!)
			//so if that other node is us we can skip this channel as well.
			if channel.Peer.PubKey().IsEqual(self) {
				log.Warnf("See its good we checked this (was going to count a channel that is to us as weight!)")
				return nil
			}

			selectionSlice = append(selectionSlice, nID)
			// nodeWeightsByNodeID[nID] += channel.Capacity

			return nil
		}); err != nil {
			return err
		}

		return nil
	}); err != nil {
		return nil, err
	}
	if len(selectionSlice) == 0 {
		return directives, nil
	}

	// type weightedNode struct {
	// 	Weight btcutil.Amount //its just an int64 and now i dont have to typeassert(?is that the right term)
	// 	NodeID NodeID
	// }
	// nodeWeights := make([]weightedNode, len(nodeWeightsByNodeID))
	// nodeInd := 0

	// totalWeight := btcutil.Amount(0)
	// for nID, weight := range nodeWeightsByNodeID {
	// 	totalWeight += weight
	// 	nodeWeights[nodeInd] = weightedNode{weight, nID}
	// }

	// func RandomWeightedSelect(games []Game, totalWeight int) (Game, error) {
	//     r := prand.Intn(totalWeight)
	//     for _, g := range nodeWeights {
	//         r -= g.Weight
	//         if r <= 0 {
	//             return g, nil
	//         }
	//     }
	//     return Game{}, errors.New("No item selected")
	// }

	//shuffle and pull out unique selected nodes up to the number we need or we run out.
	shuffler := func(candidates []NodeID) []NodeID {
		shuffledNodes := make([]NodeID, len(candidates))
		perm := prand.Perm(len(candidates))
		for i, v := range perm {
			shuffledNodes[v] = candidates[i]
		}
		return shuffledNodes
	}

	candidateNodeIds := shuffler(selectionSlice)
	log.Warnf("Select: should try to get %d unique nodes out of %d shuffled weighted candidateNodeIds.", numChansToOpen, len(candidateNodeIds))

	// candidateNodeIds := []NodeID{}
	visited := make(map[NodeID]struct{})
	candidateCount := 0
	for _, nID := range candidateNodeIds {

		// Once a node has already been attached to, we'll
		// ensure that it isn't factored into any further
		// decisions within this round.
		if _, ok := visited[nID]; ok {
			continue
		}

		selectedNode := idToNode[nID]

		// With the node selected, we'll add this (node, amount) tuple
		// to out set of recommended directives.
		pub := selectedNode.PubKey()
		directives = append(directives, AttachmentDirective{
			// TODO(roasbeef): need curve?
			PeerKey: &btcec.PublicKey{
				X: pub.X,
				Y: pub.Y,
			},
			Addrs: selectedNode.Addrs(),
		})

		visited[nID] = struct{}{}
		candidateCount += 1
		if candidateCount >= numChansToOpen {
			break
		}
	}

	numSelectedNodes := int64(len(directives))
	log.Warnf("Select: prepared %d attachment directives to attempt at this time.", numSelectedNodes)

	var debuggg = false
	if debuggg {
		log.Warnf("Select: DEBUG ACTUALLY DONT RETURN ANY DIRECTIVES!!")
		return directives, nil
	}

	///TODO maybe this can be shared code somehow because it is copypasta from the prefattach.
	switch {
	// If we have enough available funds to distribute the maximum channel
	// size for each of the selected peers to attach to, then we'll
	// allocate the maximum amount to each peer.
	case int64(fundsAvailable) >= numSelectedNodes*int64(p.maxChanSize):
		for i := 0; i < int(numSelectedNodes); i++ {
			directives[i].ChanAmt = p.maxChanSize
		}

		return directives, nil

	// Otherwise, we'll greedily allocate our funds to the channels
	// successively until we run out of available funds, or can't create a
	// channel above the min channel size.
	case int64(fundsAvailable) < numSelectedNodes*int64(p.maxChanSize):
		i := 0
		for fundsAvailable > p.minChanSize {
			// We'll attempt to allocate the max channel size
			// initially. If we don't have enough funds to do this,
			// then we'll allocate the remainder of the funds
			// available to the channel.
			delta := p.maxChanSize
			if fundsAvailable-delta < 0 {
				delta = fundsAvailable
			}

			directives[i].ChanAmt = delta

			fundsAvailable -= delta
			i++
		}

		// We'll slice the initial set of directives to properly
		// reflect the amount of funds we were able to allocate.
		return directives[:i:i], nil

	default:
		return nil, fmt.Errorf("err")
	}
}
