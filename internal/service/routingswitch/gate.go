package routingswitch

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

type OperationKind string

const (
	OperationRoute    OperationKind = "route"
	OperationTraffic  OperationKind = "traffic"
	OperationRecovery OperationKind = "recovery"
)

var (
	ErrRouteOperationBusy = errors.New("route_operation_busy")
	ErrTokenReleased      = errors.New("route_operation_token_released")
	ErrTokenNotOwner      = errors.New("route_operation_token_not_owner")
)

// Gate serializes only the ownership decision. Callers must release the token
// before doing any I/O and must not acquire another App mutex while holding
// the Gate's private mutex.
type Gate struct {
	mu         sync.Mutex
	next       uint64
	active     uint64
	activeKind OperationKind
}

func NewGate() *Gate { return &Gate{} }

type Token struct {
	gate     *Gate
	id       uint64
	kind     OperationKind
	released atomic.Bool
}

func (g *Gate) Begin(kind OperationKind) (Token, error) {
	if g == nil || !validOperationKind(kind) {
		return Token{}, ErrRouteOperationBusy
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != 0 {
		return Token{}, ErrRouteOperationBusy
	}
	g.next++
	if g.next == 0 {
		g.next++
	}
	g.active = g.next
	g.activeKind = kind
	return Token{gate: g, id: g.active, kind: kind}, nil
}

func (t *Token) Release() error {
	if t == nil || t.gate == nil {
		return ErrTokenNotOwner
	}
	if !t.released.CompareAndSwap(false, true) {
		return ErrTokenReleased
	}
	t.gate.mu.Lock()
	defer t.gate.mu.Unlock()
	if t.gate.active != t.id || t.gate.activeKind != t.kind {
		return ErrTokenNotOwner
	}
	t.gate.active = 0
	t.gate.activeKind = ""
	return nil
}

func (t Token) Kind() OperationKind { return t.kind }

func (g *Gate) Active() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active != 0
}

func validOperationKind(kind OperationKind) bool {
	switch kind {
	case OperationRoute, OperationTraffic, OperationRecovery:
		return true
	default:
		return false
	}
}

// TransitionID is an opaque route epoch. Its value must never be interpreted
// as a backup, gateway, capture, or user identity.
type TransitionID string

var (
	ErrInvalidTransition = errors.New("invalid_transition")
	ErrStaleTransition   = errors.New("stale_transition")
)

func (id TransitionID) Valid() bool {
	if id == "" {
		return false
	}
	_, err := uuid.Parse(string(id))
	return err == nil
}

// Generator is instance-scoped so tests and App instances do not share a
// mutable package-level hook.
type Generator struct {
	newID func() uuid.UUID
}

func NewGenerator() *Generator {
	return &Generator{newID: uuid.New}
}

func (g *Generator) New() (TransitionID, error) {
	if g == nil || g.newID == nil {
		return "", ErrInvalidTransition
	}
	id := TransitionID(g.newID().String())
	if !id.Valid() {
		return "", ErrInvalidTransition
	}
	return id, nil
}

// ValidateTransition rejects missing, malformed, or stale route epochs before
// any mutation is attempted.
func ValidateTransition(provided, current, journal TransitionID) error {
	if !provided.Valid() || !current.Valid() || !journal.Valid() {
		return ErrInvalidTransition
	}
	if provided != current || provided != journal {
		return ErrStaleTransition
	}
	return nil
}
