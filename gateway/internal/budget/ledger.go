// 本包以万分之一元为单位原子预留、结算每日预算，并持久化已用额度。
package budget

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	ErrRequestLimitExceeded = errors.New("request cost limit exceeded")
	ErrDailyBudgetExceeded  = errors.New("daily budget exceeded")
	ErrReservationMissing   = errors.New("budget reservation not found")
)

type stateFile struct {
	Day          string                 `json:"day"`
	SpentUnits   int64                  `json:"spent_units"`
	Reservations map[string]reservation `json:"reservations"`
}

type reservation struct {
	Units int64  `json:"units"`
	Day   string `json:"day"`
}

// Snapshot 是预算检查的只读状态。
type Snapshot struct {
	Day              string
	SpentYuan        float64
	ReservedYuan     float64
	DailyLimitYuan   float64
	RequestLimitYuan float64
	RemainingYuan    float64
}

// Ledger 在并发请求间协调额度预留，并将每日用量保存到本地 JSON 状态文件。
type Ledger struct {
	mu                sync.Mutex
	dailyLimitUnits   int64
	requestLimitUnits int64
	day               string
	spentUnits        int64
	reservations      map[string]reservation
	statePath         string
}

// New 载入预算状态；若进程异常退出，未结算预留按已花费额度处理以免重启绕过预算。
func New(dailyLimitYuan, requestLimitYuan float64, statePath string) (*Ledger, error) {
	dailyLimit, err := toUnits(dailyLimitYuan)
	if err != nil || dailyLimit <= 0 {
		return nil, fmt.Errorf("daily budget must be positive and finite")
	}
	requestLimit, err := toUnits(requestLimitYuan)
	if err != nil || requestLimit <= 0 {
		return nil, fmt.Errorf("request cost limit must be positive and finite")
	}
	if statePath == "" {
		return nil, errors.New("budget state path must not be empty")
	}
	ledger := &Ledger{
		dailyLimitUnits:   dailyLimit,
		requestLimitUnits: requestLimit,
		day:               localDay(time.Now()),
		reservations:      make(map[string]reservation),
		statePath:         statePath,
	}
	state, err := readState(statePath)
	if err != nil {
		return nil, err
	}
	if state != nil && state.Day == ledger.day {
		ledger.spentUnits = max(state.SpentUnits, 0)
		for _, reserved := range state.Reservations {
			// 进程重启后无法判断请求是否已被上游处理，按预留上限计入当天支出。
			if reserved.Units > 0 {
				ledger.spentUnits = safeAdd(ledger.spentUnits, reserved.Units)
			}
		}
	}
	if state != nil && state.Day == ledger.day && len(state.Reservations) > 0 {
		if err := ledger.persistLocked(); err != nil {
			return nil, fmt.Errorf("persist recovered budget reservations: %w", err)
		}
	}
	return ledger, nil
}

// Reserve 在请求开始前原子预留估算上限，阻止并发调用共同突破日预算。
func (l *Ledger) Reserve(traceID string, estimateYuan float64) error {
	units, err := toUnits(estimateYuan)
	if err != nil || units < 0 {
		return fmt.Errorf("invalid estimated request cost")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rotateDayLocked(time.Now()); err != nil {
		return err
	}
	if units > l.requestLimitUnits {
		return ErrRequestLimitExceeded
	}
	if _, exists := l.reservations[traceID]; exists {
		return fmt.Errorf("budget reservation already exists for trace")
	}
	reserved := l.reservedUnitsLocked()
	if exceedsLimit(l.spentUnits, reserved, units, l.dailyLimitUnits) {
		return ErrDailyBudgetExceeded
	}
	l.reservations[traceID] = reservation{Units: units, Day: l.day}
	if err := l.persistLocked(); err != nil {
		delete(l.reservations, traceID)
		return fmt.Errorf("persist budget reservation: %w", err)
	}
	return nil
}

// Settle 用 Provider usage 的真实成本替换预留额，并检查是否突破预算。
func (l *Ledger) Settle(traceID string, actualYuan float64) error {
	units, err := toUnits(actualYuan)
	if err != nil || units < 0 {
		return fmt.Errorf("invalid actual request cost")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rotateDayLocked(time.Now()); err != nil {
		return err
	}
	reservation, exists := l.reservations[traceID]
	if !exists {
		return ErrReservationMissing
	}
	oldSpent := l.spentUnits
	delete(l.reservations, traceID)
	if reservation.Day == l.day {
		l.spentUnits = safeAdd(l.spentUnits, units)
	}
	if err := l.persistLocked(); err != nil {
		l.reservations[traceID] = reservation
		l.spentUnits = oldSpent
		return fmt.Errorf("persist settled budget: %w", err)
	}
	if exceedsLimit(l.spentUnits, l.reservedUnitsLocked(), 0, l.dailyLimitUnits) {
		return ErrDailyBudgetExceeded
	}
	if units > l.requestLimitUnits {
		return ErrRequestLimitExceeded
	}
	return nil
}

// Release 释放确定未产生费用的请求预留。
func (l *Ledger) Release(traceID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.reservations[traceID]; !exists {
		return ErrReservationMissing
	}
	reservation := l.reservations[traceID]
	delete(l.reservations, traceID)
	if err := l.persistLocked(); err != nil {
		l.reservations[traceID] = reservation
		return fmt.Errorf("persist released budget reservation: %w", err)
	}
	return nil
}

// ConsumeReservation 将无法取得 usage 的不确定请求按完整预留额计入预算。
func (l *Ledger) ConsumeReservation(traceID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rotateDayLocked(time.Now()); err != nil {
		return err
	}
	reservation, exists := l.reservations[traceID]
	if !exists {
		return ErrReservationMissing
	}
	oldSpent := l.spentUnits
	delete(l.reservations, traceID)
	if reservation.Day == l.day {
		l.spentUnits = safeAdd(l.spentUnits, reservation.Units)
	}
	if err := l.persistLocked(); err != nil {
		l.reservations[traceID] = reservation
		l.spentUnits = oldSpent
		return fmt.Errorf("persist consumed budget reservation: %w", err)
	}
	return nil
}

// Snapshot 返回当前日期的已用、预留和剩余额度。
func (l *Ledger) Snapshot() (Snapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rotateDayLocked(time.Now()); err != nil {
		return Snapshot{}, err
	}
	spent := float64(l.spentUnits) / 10_000
	reserved := float64(l.reservedUnitsLocked()) / 10_000
	dailyLimit := float64(l.dailyLimitUnits) / 10_000
	return Snapshot{
		Day:              l.day,
		SpentYuan:        spent,
		ReservedYuan:     reserved,
		DailyLimitYuan:   dailyLimit,
		RequestLimitYuan: float64(l.requestLimitUnits) / 10_000,
		RemainingYuan:    max(0, dailyLimit-spent-reserved),
	}, nil
}

// SnapshotLimits 返回固定配置的额度上限，不读取或修改持久化状态。
func (l *Ledger) SnapshotLimits() Snapshot {
	return Snapshot{
		DailyLimitYuan:   float64(l.dailyLimitUnits) / 10_000,
		RequestLimitYuan: float64(l.requestLimitUnits) / 10_000,
	}
}

func (l *Ledger) rotateDayLocked(now time.Time) error {
	today := localDay(now)
	if today == l.day {
		return nil
	}
	oldDay, oldSpent := l.day, l.spentUnits
	l.day = today
	l.spentUnits = 0
	if err := l.persistLocked(); err != nil {
		l.day, l.spentUnits = oldDay, oldSpent
		return fmt.Errorf("persist daily budget reset: %w", err)
	}
	return nil
}

func (l *Ledger) reservedUnitsLocked() int64 {
	var total int64
	for _, reservation := range l.reservations {
		if reservation.Day == l.day && reservation.Units > 0 {
			total = safeAdd(total, reservation.Units)
		}
	}
	return total
}

func (l *Ledger) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(l.statePath), 0o700); err != nil {
		return err
	}
	temporary := l.statePath + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encodeErr := json.NewEncoder(file).Encode(stateFile{
		Day:          l.day,
		SpentUnits:   l.spentUnits,
		Reservations: l.reservations,
	})
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(encodeErr, syncErr, closeErr); err != nil {
		removeErr := os.Remove(temporary)
		return errors.Join(err, ignoreNotExist(removeErr))
	}
	if err := os.Rename(temporary, l.statePath); err != nil {
		removeErr := os.Remove(temporary)
		return errors.Join(err, ignoreNotExist(removeErr))
	}
	return nil
}

func readState(path string) (*stateFile, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open budget state: %w", err)
	}
	var state stateFile
	decodeErr := json.NewDecoder(file).Decode(&state)
	closeErr := file.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return nil, fmt.Errorf("read budget state: %w", err)
	}
	if state.Day == "" || state.SpentUnits < 0 || state.Reservations == nil {
		return nil, errors.New("budget state is invalid")
	}
	for _, units := range state.Reservations {
		if units.Units < 0 || units.Day == "" {
			return nil, errors.New("budget state contains a negative reservation")
		}
	}
	return &state, nil
}

func toUnits(yuan float64) (int64, error) {
	scaled := yuan * 10_000
	if yuan < 0 || math.IsNaN(yuan) || math.IsInf(yuan, 0) ||
		math.IsInf(scaled, 0) || math.Round(scaled) >= float64(math.MaxInt64) {
		return 0, errors.New("yuan amount must be finite and non-negative")
	}
	return int64(math.Round(scaled)), nil
}

func localDay(value time.Time) string {
	return value.Format("2006-01-02")
}

func safeAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func exceedsLimit(spent, reserved, additional, limit int64) bool {
	if spent > limit || reserved > limit-spent {
		return true
	}
	return additional > limit-spent-reserved
}

func ignoreNotExist(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
