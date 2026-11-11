package budget

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestReserveRejectsPerRequestAndDailyLimits(t *testing.T) {
	ledger, err := New(1, 0.5, filepath.Join(t.TempDir(), "budget.json"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := ledger.Reserve("too-large", 0.5001); !errors.Is(err, ErrRequestLimitExceeded) {
		t.Fatalf("Reserve() error = %v, want request limit", err)
	}
	if err := ledger.Reserve("first", 0.5); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if err := ledger.Reserve("second", 0.5); err != nil {
		t.Fatalf("Reserve() second error = %v", err)
	}
	if err := ledger.Reserve("third", 0.0001); !errors.Is(err, ErrDailyBudgetExceeded) {
		t.Fatalf("Reserve() error = %v, want daily limit", err)
	}
}

func TestReservationsAreConcurrentAndSettledUsageReleasesUnusedAmount(t *testing.T) {
	ledger, err := New(1, 0.5, filepath.Join(t.TempDir(), "budget.json"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var wait sync.WaitGroup
	var successes int
	var lock sync.Mutex
	accepted := make([]string, 0, 5)
	for i := 0; i < 10; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			traceID := string(rune('a' + i))
			if err := ledger.Reserve(traceID, 0.2); err == nil {
				lock.Lock()
				successes++
				accepted = append(accepted, traceID)
				lock.Unlock()
			}
		}(i)
	}
	wait.Wait()
	if successes != 5 {
		t.Fatalf("successful reservations = %d, want 5", successes)
	}
	if err := ledger.Settle(accepted[0], 0.0001); err != nil {
		t.Fatalf("Settle() error = %v", err)
	}
	snapshot, err := ledger.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.SpentYuan != 0.0001 || snapshot.ReservedYuan != 0.8 {
		t.Fatalf("unexpected budget snapshot: %+v", snapshot)
	}
}

func TestLedgerPersistsSpentBudgetAcrossRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "budget.json")
	ledger, err := New(1, 0.5, statePath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := ledger.Reserve("trace", 0.2); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if err := ledger.Settle("trace", 0.1); err != nil {
		t.Fatalf("Settle() error = %v", err)
	}
	restarted, err := New(1, 0.5, statePath)
	if err != nil {
		t.Fatalf("New() after restart error = %v", err)
	}
	snapshot, err := restarted.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if got := snapshot.SpentYuan; got != 0.1 {
		t.Fatalf("persisted spent cost = %.4f, want 0.1000", got)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("budget state missing: %v", err)
	}
}

func TestRestartConsumesOutstandingReservationConservatively(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "budget.json")
	ledger, err := New(1, 0.5, statePath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := ledger.Reserve("inflight", 0.25); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	restarted, err := New(1, 0.5, statePath)
	if err != nil {
		t.Fatalf("New() after restart error = %v", err)
	}
	snapshot, err := restarted.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.SpentYuan != 0.25 || snapshot.ReservedYuan != 0 {
		t.Fatalf("unexpected recovery snapshot: %+v", snapshot)
	}
}

func TestSettleRecordsActualOverLimitCostAndRejectsResponse(t *testing.T) {
	ledger, err := New(1, 0.5, filepath.Join(t.TempDir(), "budget.json"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := ledger.Reserve("trace", 0.2); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if err := ledger.Settle("trace", 0.6); !errors.Is(err, ErrRequestLimitExceeded) {
		t.Fatalf("Settle() error = %v, want request limit exceeded", err)
	}
	snapshot, err := ledger.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.SpentYuan != 0.6 {
		t.Fatalf("spent cost = %.4f, want actual 0.6000 to remain recorded", snapshot.SpentYuan)
	}
}

func TestPersistenceFailureRollsBackReservationAndSettlement(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "budget.json")
	ledger, err := New(1, 0.5, statePath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := ledger.Reserve("trace", 0.2); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}

	ledger.statePath = t.TempDir()
	if err := ledger.Settle("trace", 0.1); err == nil {
		t.Fatal("Settle() succeeded with an unwritable state path")
	}
	if _, exists := ledger.reservations["trace"]; !exists {
		t.Fatal("failed settlement discarded the durable reservation")
	}
	if ledger.spentUnits != 0 {
		t.Fatalf("spent units after failed settlement = %d, want 0", ledger.spentUnits)
	}
}

func TestDayRotationFailureKeepsPreviousBudgetState(t *testing.T) {
	ledger, err := New(1, 0.5, filepath.Join(t.TempDir(), "budget.json"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	previousDay := "2000-01-01"
	ledger.day = previousDay
	ledger.spentUnits = 2500
	ledger.statePath = t.TempDir()

	if _, err := ledger.Snapshot(); err == nil {
		t.Fatal("Snapshot() succeeded with an unwritable state path")
	}
	if ledger.day != previousDay || ledger.spentUnits != 2500 {
		t.Fatalf("failed day rotation changed state: day=%s spent=%d", ledger.day, ledger.spentUnits)
	}
}
