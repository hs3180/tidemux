package observability

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"sync/atomic"
)

var (
	ErrRuntimeIdentity        = errors.New("runtime identity initialization failed")
	ErrEventIdentityExhausted = errors.New("runtime event identity exhausted")
	serviceVersionPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}$`)
	processIdentityOnce       sync.Once
	processIdentity           *runtimeIdentity
	processIdentityError      error
)

// The random process namespace and monotonic counter need constant space.
// They are shared by all loggers in this process, including early startup.
type runtimeIdentity struct {
	instance string
	sequence atomic.Uint64
}

func newRuntimeIdentity(entropy io.Reader) (*runtimeIdentity, error) {
	var value [16]byte
	if _, err := io.ReadFull(entropy, value[:]); err != nil {
		return nil, ErrRuntimeIdentity
	}
	return &runtimeIdentity{instance: hex.EncodeToString(value[:])}, nil
}

func (i *runtimeIdentity) eventID() (string, error) {
	for {
		previous := i.sequence.Load()
		if previous == ^uint64(0) {
			return "", ErrEventIdentityExhausted
		}
		if i.sequence.CompareAndSwap(previous, previous+1) {
			return fmt.Sprintf("%s-%016x", i.instance, previous+1), nil
		}
	}
}

type serviceMetadata struct {
	Name     string          `json:"name"`
	Version  string          `json:"version"`
	Instance serviceInstance `json:"instance"`
}

type serviceInstance struct {
	ID string `json:"id"`
}

func processRuntimeIdentity() (*runtimeIdentity, error) {
	processIdentityOnce.Do(func() {
		processIdentity, processIdentityError = newRuntimeIdentity(rand.Reader)
	})
	return processIdentity, processIdentityError
}
