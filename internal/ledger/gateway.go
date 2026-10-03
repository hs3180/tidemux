package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// OpenForGateway claims exclusive gateway ownership before initializing the
// database and recovering abandoned attempts. Ordinary Open calls never recover
// pending rows. Canonicalize symlinks so path aliases share the lock. A separate
// file avoids interference with SQLite's own database locks on macOS. Never
// unlink it: the OS releases ownership even if the owning process crashes.
func OpenForGateway(path string) (*Ledger, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("cannot resolve gateway ledger")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if os.IsNotExist(err) {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		if parentErr != nil {
			return nil, errors.New("cannot resolve gateway ledger directory")
		}
		canonical, err = filepath.Join(parent, filepath.Base(absolute)), nil
	}
	if err != nil {
		return nil, errors.New("cannot resolve gateway ledger")
	}
	owner, err := os.OpenFile(canonical+".gateway.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("cannot claim gateway ledger")
	}
	if err := syscall.Flock(int(owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		owner.Close()
		return nil, errors.New("ledger is already owned by a gateway or cannot be locked")
	}
	l, err := Open(canonical)
	if err != nil {
		owner.Close()
		return nil, err
	}
	l.owner = owner
	if _, err := l.db.ExecContext(context.Background(), `UPDATE budget_charges SET state='unknown' WHERE state='pending'`); err != nil {
		l.Close()
		return nil, errors.New("cannot recover abandoned budget attempts")
	}
	return l, nil
}
