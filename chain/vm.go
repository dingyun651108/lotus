package chain

import (
	"context"
	"fmt"

	"github.com/filecoin-project/go-lotus/chain/actors"
	"github.com/filecoin-project/go-lotus/chain/address"
	"github.com/filecoin-project/go-lotus/chain/types"
	"github.com/filecoin-project/go-lotus/lib/bufbstore"
	"golang.org/x/xerrors"

	bserv "github.com/ipfs/go-blockservice"
	cid "github.com/ipfs/go-cid"
	hamt "github.com/ipfs/go-hamt-ipld"
	ipld "github.com/ipfs/go-ipld-format"
	dag "github.com/ipfs/go-merkledag"
	"github.com/pkg/errors"
)

type VMContext struct {
	vm     *VM
	state  *StateTree
	msg    *types.Message
	height uint64
	cst    *hamt.CborIpldStore

	// root cid of the state of the actor this invocation will be on
	sroot cid.Cid

	storage types.Storage
}

// Message is the message that kicked off the current invocation
func (vmc *VMContext) Message() *types.Message {
	return vmc.msg
}

type storage struct {
	// would be great to stop depending on this crap everywhere
	// I am my own worst enemy
	cst  *hamt.CborIpldStore
	head cid.Cid
}

func (s *storage) Put(i interface{}) (cid.Cid, error) {
	return s.cst.Put(context.TODO(), i)
}

func (s *storage) Get(c cid.Cid, out interface{}) error {
	return s.cst.Get(context.TODO(), c, out)
}

func (s *storage) GetHead() cid.Cid {
	return s.head
}

func (s *storage) Commit(oldh, newh cid.Cid) error {
	if s.head != oldh {
		return fmt.Errorf("failed to update, inconsistent base reference")
	}

	s.head = newh
	return nil
}

// Storage provides access to the VM storage layer
func (vmc *VMContext) Storage() types.Storage {
	return vmc.storage
}

func (vmc *VMContext) Ipld() *hamt.CborIpldStore {
	return vmc.cst
}

// Send allows the current execution context to invoke methods on other actors in the system
func (vmc *VMContext) Send(to address.Address, method uint64, value types.BigInt, params []byte) ([]byte, uint8, error) {
	msg := &types.Message{
		From:   vmc.msg.From,
		To:     to,
		Method: method,
		Value:  value,
		Params: params,
	}

	toAct, err := vmc.state.GetActor(to)
	if err != nil {
		return nil, 0, err
	}

	nvmctx := vmc.vm.makeVMContext(toAct.Head, msg)

	res, ret, err := vmc.vm.Invoke(toAct, nvmctx, method, params)
	if err != nil {
		return nil, 0, err
	}

	toAct.Head = nvmctx.Storage().GetHead()

	return res, ret, err
}

// BlockHeight returns the height of the block this message was added to the chain in
func (vmc *VMContext) BlockHeight() uint64 {
	return vmc.height
}

func (vmc *VMContext) GasUsed() types.BigInt {
	return types.NewInt(0)
}

func (vmc *VMContext) StateTree() (types.StateTree, error) {
	if vmc.msg.To != actors.InitActorAddress {
		return nil, fmt.Errorf("only init actor can access state tree directly")
	}

	return vmc.state, nil
}

func (vm *VM) makeVMContext(sroot cid.Cid, msg *types.Message) *VMContext {
	cst := hamt.CSTFromBstore(vm.cs.bs)

	return &VMContext{
		vm:     vm,
		state:  vm.cstate,
		sroot:  sroot,
		msg:    msg,
		height: vm.blockHeight,
		cst:    cst,
		storage: &storage{
			cst:  cst,
			head: sroot,
		},
	}
}

type VM struct {
	cstate      *StateTree
	base        cid.Cid
	cs          *ChainStore
	buf         *bufbstore.BufferedBS
	blockHeight uint64
	blockMiner  address.Address
	inv         *invoker
}

func NewVM(base cid.Cid, height uint64, maddr address.Address, cs *ChainStore) (*VM, error) {
	buf := bufbstore.NewBufferedBstore(cs.bs)
	cst := hamt.CSTFromBstore(buf)
	state, err := LoadStateTree(cst, base)
	if err != nil {
		return nil, err
	}

	return &VM{
		cstate:      state,
		base:        base,
		cs:          cs,
		buf:         buf,
		blockHeight: height,
		blockMiner:  maddr,
		inv:         newInvoker(),
	}, nil
}

func (vm *VM) ApplyMessage(msg *types.Message) (*types.MessageReceipt, error) {
	st := vm.cstate
	st.Snapshot()
	fromActor, err := st.GetActor(msg.From)
	if err != nil {
		return nil, errors.Wrap(err, "from actor not found")
	}

	gascost := types.BigMul(msg.GasLimit, msg.GasPrice)
	totalCost := types.BigAdd(gascost, msg.Value)
	if types.BigCmp(fromActor.Balance, totalCost) < 0 {
		return nil, fmt.Errorf("not enough funds")
	}

	if msg.Nonce != fromActor.Nonce {
		return nil, fmt.Errorf("invalid nonce")
	}
	fromActor.Nonce++

	toActor, err := st.GetActor(msg.To)
	if err != nil {
		if err == ErrActorNotFound {
			a, err := TryCreateAccountActor(st, msg.To)
			if err != nil {
				return nil, err
			}
			toActor = a
		} else {
			return nil, err
		}
	}

	if err := DeductFunds(fromActor, totalCost); err != nil {
		return nil, errors.Wrap(err, "failed to deduct funds")
	}
	DepositFunds(toActor, msg.Value)

	vmctx := vm.makeVMContext(toActor.Head, msg)

	var errcode byte
	var ret []byte
	if msg.Method != 0 {
		ret, errcode, err = vm.Invoke(toActor, vmctx, msg.Method, msg.Params)
		if err != nil {
			return nil, err
		}

		if errcode != 0 {
			// revert all state changes since snapshot
			st.Revert()
			gascost := types.BigMul(vmctx.GasUsed(), msg.GasPrice)
			if err := DeductFunds(fromActor, gascost); err != nil {
				panic("invariant violated: " + err.Error())
			}
		} else {
			// refund unused gas
			refund := types.BigMul(types.BigSub(msg.GasLimit, vmctx.GasUsed()), msg.GasPrice)
			DepositFunds(fromActor, refund)
		}
	}

	// reward miner gas fees
	miner, err := st.GetActor(vm.blockMiner)
	if err != nil {
		return nil, errors.Wrap(err, "getting block miner actor failed")
	}

	gasReward := types.BigMul(msg.GasPrice, vmctx.GasUsed())
	DepositFunds(miner, gasReward)

	return &types.MessageReceipt{
		ExitCode: errcode,
		Return:   ret,
		GasUsed:  vmctx.GasUsed(),
	}, nil
}

func (vm *VM) Flush(ctx context.Context) (cid.Cid, error) {
	from := dag.NewDAGService(bserv.New(vm.buf, nil))
	to := dag.NewDAGService(bserv.New(vm.buf.Read(), nil))

	root, err := vm.cstate.Flush()
	if err != nil {
		return cid.Undef, xerrors.Errorf("flushing vm: %w", err)
	}

	if err := Copy(ctx, from, to, root); err != nil {
		return cid.Undef, xerrors.Errorf("copying tree: %w", err)
	}

	return root, nil
}

func Copy(ctx context.Context, from, to ipld.DAGService, root cid.Cid) error {
	if root.Prefix().MhType == 0 {
		// identity cid, skip
		return nil
	}
	node, err := from.Get(ctx, root)
	if err != nil {
		fmt.Printf("fail: %#v\n", root.Prefix())
		return errors.Wrapf(err, "get %s", root)
	}
	links := node.Links()
	for _, link := range links {
		_, err := to.Get(ctx, link.Cid)
		switch err {
		default:
			return err
		case nil:
			continue
		case ipld.ErrNotFound:
			// continue
		}
		if err := Copy(ctx, from, to, link.Cid); err != nil {
			return err
		}
	}
	err = to.Add(ctx, node)
	if err != nil {
		return err
	}
	return nil
}

func (vm *VM) TransferFunds(from, to address.Address, amt types.BigInt) error {
	if from == to {
		return nil
	}

	fromAct, err := vm.cstate.GetActor(from)
	if err != nil {
		return err
	}

	toAct, err := vm.cstate.GetActor(from)
	if err != nil {
		return err
	}

	if err := DeductFunds(fromAct, amt); err != nil {
		return errors.Wrap(err, "failed to deduct funds")
	}
	DepositFunds(toAct, amt)

	return nil
}

func (vm *VM) Invoke(act *types.Actor, vmctx *VMContext, method uint64, params []byte) ([]byte, byte, error) {
	ret, err := vm.inv.Invoke(act, vmctx, method, params)
	if err != nil {
		return nil, 0, err
	}
	return ret.Result, ret.ReturnCode, nil
}
