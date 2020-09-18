package migration

import (
	"context"
	"sync"

	address "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	builtin0 "github.com/filecoin-project/specs-actors/actors/builtin"
	states0 "github.com/filecoin-project/specs-actors/actors/states"
	"github.com/filecoin-project/specs-actors/actors/util/adt"
	"github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	"golang.org/x/sync/semaphore"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/specs-actors/v2/actors/builtin"
	"github.com/filecoin-project/specs-actors/v2/actors/states"
)

var (
	maxWorkers = 32 // TODO evaluate empirically
	sem        *semaphore.Weighted
	migMu      = &sync.Mutex{}
	actOutMu   = &sync.Mutex{}
)

type StateMigration interface {
	// Loads an actor's state from an input store and writes new state to an output store.
	// Returns the new state head CID.
	MigrateState(ctx context.Context, store cbor.IpldStore, head cid.Cid) (cid.Cid, error)
}

type ActorMigration struct {
	OutCodeCID     cid.Cid
	StateMigration StateMigration
}

var migrations = map[cid.Cid]ActorMigration{ // nolint:varcheck,deadcode,unused
	builtin0.AccountActorCodeID: ActorMigration{
		OutCodeCID:     builtin.AccountActorCodeID,
		StateMigration: &accountMigrator{},
	},
	builtin0.CronActorCodeID: ActorMigration{
		OutCodeCID:     builtin.CronActorCodeID,
		StateMigration: &cronMigrator{},
	},
	builtin0.InitActorCodeID: ActorMigration{
		OutCodeCID:     builtin.InitActorCodeID,
		StateMigration: &initMigrator{},
	},
	builtin0.StorageMarketActorCodeID: ActorMigration{
		OutCodeCID:     builtin.StorageMarketActorCodeID,
		StateMigration: &marketMigrator{},
	},
	builtin0.MultisigActorCodeID: ActorMigration{
		OutCodeCID:     builtin.MultisigActorCodeID,
		StateMigration: &multisigMigrator{},
	},
	builtin0.PaymentChannelActorCodeID: ActorMigration{
		OutCodeCID:     builtin.PaymentChannelActorCodeID,
		StateMigration: &paychMigrator{},
	},
	builtin0.StoragePowerActorCodeID: ActorMigration{
		OutCodeCID:     builtin.StoragePowerActorCodeID,
		StateMigration: &powerMigrator{},
	},
	builtin0.RewardActorCodeID: ActorMigration{
		OutCodeCID:     builtin.RewardActorCodeID,
		StateMigration: &rewardMigrator{},
	},
	builtin0.SystemActorCodeID: ActorMigration{
		OutCodeCID:     builtin.SystemActorCodeID,
		StateMigration: &systemMigrator{},
	},
	builtin0.VerifiedRegistryActorCodeID: ActorMigration{
		OutCodeCID:     builtin.VerifiedRegistryActorCodeID,
		StateMigration: &verifregMigrator{},
	},
}

type MinerStateMigration interface {
	MigrateState(ctx context.Context, store cbor.IpldStore, head cid.Cid, balance abi.TokenAmount) (cid.Cid, abi.TokenAmount, error)
}

type MinerMigration struct {
	OutCodeCID          cid.Cid
	MinerStateMigration MinerStateMigration
}

var minerMigration = MinerMigration{
	OutCodeCID:          builtin.StorageMinerActorCodeID,
	MinerStateMigration: &minerMigrator{},
}

func migrateOneActor(ctx context.Context, store cbor.IpldStore, addr address.Address, actorIn states.Actor, actorsOut *states.Tree, transferCh chan big.Int, errCh chan error) {
	var headOut, codeOut cid.Cid
	var err error
	transfer := big.Zero()
	// This will be migrated at the end
	if actorIn.Code == builtin0.VerifiedRegistryActorCodeID {
		sem.Release(1)
		return
	} else if actorIn.Code == builtin0.StorageMinerActorCodeID {
		codeOut = minerMigration.OutCodeCID
		headOut, transfer, err = minerMigration.MinerStateMigration.MigrateState(ctx, store, actorIn.Head, actorIn.Balance)
	} else {
		migMu.Lock()
		migration := migrations[actorIn.Code]
		migMu.Unlock()
		codeOut = migration.OutCodeCID
		headOut, err = migration.StateMigration.MigrateState(ctx, store, actorIn.Head)
	}

	if err != nil {
		err = xerrors.Errorf("state migration error on %s actor at addr %s: %w", builtin.ActorNameByCode(codeOut), addr, err)
		sem.Release(1)
		errCh <- err
		return
	}

	// set up new state root with the migrated state
	actorOut := states.Actor{
		Code:       codeOut,
		Head:       headOut,
		CallSeqNum: actorIn.CallSeqNum,
		Balance:    big.Add(actorIn.Balance, transfer),
	}
	actOutMu.Lock()
	err = actorsOut.SetActor(addr, &actorOut)
	actOutMu.Unlock()
	if err != nil {
		sem.Release(1)
		errCh <- err
		return
	}
	if transfer.GreaterThan(big.Zero()) {
		sem.Release(1)
		transferCh <- transfer
		return
	}
	sem.Release(1)
	return // nolint:gosimple
}

// Migrates the filecoin state tree starting from the global state tree and upgrading all actor state.
func MigrateStateTree(ctx context.Context, store cbor.IpldStore, stateRootIn cid.Cid) (cid.Cid, error) {
	// Setup input and output state tree helpers
	adtStore := adt.WrapStore(ctx, store)
	actorsIn, err := states0.LoadTree(adtStore, stateRootIn)
	if err != nil {
		return cid.Undef, err
	}
	stateRootOut, err := adt.MakeEmptyMap(adtStore).Root()
	if err != nil {
		return cid.Undef, err
	}
	actorsOut, err := states.LoadTree(adtStore, stateRootOut)
	if err != nil {
		return cid.Undef, err
	}

	// Extra actor setup
	// power
	migMu.Lock()
	pm := migrations[builtin0.StoragePowerActorCodeID].StateMigration.(*powerMigrator)
	migMu.Unlock()
	pm.actorsIn = actorsIn
	// miner
	transferFromBurnt := big.Zero()

	// Setup synchronization
	sem = semaphore.NewWeighted(int64(maxWorkers)) // reset global for each invocation
	errCh := make(chan error)
	transferCh := make(chan big.Int)

	// Iterate all actors in old state root
	// Set new state root actors as we go
	err = actorsIn.ForEach(func(addr address.Address, actorIn *states.Actor) error {
		// Read from err and transfer channels without blocking.
		// Terminate on the first error.
		// Accumulate funds transfered from burnt to miners.
	READLOOP:
		for {
			select {
			case err := <-errCh:
				return err
			case transfer := <-transferCh:
				transferFromBurnt = big.Add(transferFromBurnt, transfer)
			default:
				break READLOOP
			}
		}

		// Hand off migration of one actor, blocking if we are out of worker goroutines
		if err := sem.Acquire(ctx, 1); err != nil {
			return err
		}
		go migrateOneActor(ctx, store, addr, *actorIn, actorsOut, transferCh, errCh)
		return nil
	})
	if err != nil {
		return cid.Undef, err
	}
	// Wait on all jobs finishing
	if err := sem.Acquire(ctx, int64(maxWorkers)); err != nil {
		return cid.Undef, xerrors.Errorf("failed to wait for all worker jobs: %w", err)
	}
	// Check for outstanding transfers and errors
READEND:
	for {
		select {
		case err := <-errCh:
			return cid.Undef, err
		case transfer := <-transferCh:
			transferFromBurnt = big.Add(transferFromBurnt, transfer)
		default:
			break READEND
		}
	}

	// Migrate verified registry
	migMu.Lock()
	vm := migrations[builtin0.VerifiedRegistryActorCodeID].StateMigration.(*verifregMigrator)
	migMu.Unlock()
	vm.actorsOut = actorsOut
	verifRegActorIn, found, err := actorsIn.GetActor(builtin0.VerifiedRegistryActorAddr)
	if err != nil {
		return cid.Undef, err
	}
	if !found {
		return cid.Undef, xerrors.Errorf("could not find verifreg actor in state")
	}
	verifRegHeadOut, err := vm.MigrateState(ctx, store, verifRegActorIn.Head)
	if err != nil {
		return cid.Undef, err
	}
	verifRegActorOut := states.Actor{
		Code:       builtin.VerifiedRegistryActorCodeID,
		Head:       verifRegHeadOut,
		CallSeqNum: verifRegActorIn.CallSeqNum,
		Balance:    verifRegActorIn.Balance,
	}
	err = actorsOut.SetActor(builtin.VerifiedRegistryActorAddr, &verifRegActorOut)
	if err != nil {
		return cid.Undef, err
	}

	// Track deductions to burntFunds actor's balance
	burntFundsActor, found, err := actorsOut.GetActor(builtin.BurntFundsActorAddr)
	if err != nil {
		return cid.Undef, err
	}
	if !found {
		return cid.Undef, xerrors.Errorf("burnt funds actor not in tree")
	}
	burntFundsActor.Balance = big.Sub(burntFundsActor.Balance, transferFromBurnt)
	if burntFundsActor.Balance.LessThan(big.Zero()) {
		return cid.Undef, xerrors.Errorf("miner transfers send burnt funds actor balance below zero")
	}
	err = actorsOut.SetActor(builtin.BurntFundsActorAddr, burntFundsActor)
	if err != nil {
		return cid.Undef, err
	}

	return actorsOut.Flush()
}

func InputTreeBalance(ctx context.Context, store cbor.IpldStore, stateRootIn cid.Cid) (abi.TokenAmount, error) {
	adtStore := adt.WrapStore(ctx, store)
	actorsIn, err := states0.LoadTree(adtStore, stateRootIn)
	if err != nil {
		return big.Zero(), err
	}
	total := abi.NewTokenAmount(0)
	err = actorsIn.ForEach(func(addr address.Address, a *states.Actor) error {
		total = big.Add(total, a.Balance)
		return nil
	})
	return total, err
}
