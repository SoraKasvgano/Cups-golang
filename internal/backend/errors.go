package backend

import (
	"errors"
	"fmt"
)

type ErrorKind string

const (
	ErrorUnsupported ErrorKind = "unsupported"
	ErrorTemporary   ErrorKind = "temporary"
	ErrorPermanent   ErrorKind = "permanent"
)

type Error struct {
	Kind ErrorKind
	Op   string
	URI  string
	Err  error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		if e.Op != "" {
			return e.Op
		}
		return string(e.Kind)
	}
	if e.Op == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

var ErrUnsupported error = &Error{Kind: ErrorUnsupported, Op: "backend", Err: errors.New("backend not supported")}

func WrapUnsupported(op, uri string, err error) error {
	if err == nil {
		err = errors.New("unsupported")
	}
	return &Error{Kind: ErrorUnsupported, Op: op, URI: uri, Err: err}
}

func WrapTemporary(op, uri string, err error) error {
	if err == nil {
		err = errors.New("temporary backend error")
	}
	return &Error{Kind: ErrorTemporary, Op: op, URI: uri, Err: err}
}

func WrapPermanent(op, uri string, err error) error {
	if err == nil {
		err = errors.New("permanent backend error")
	}
	return &Error{Kind: ErrorPermanent, Op: op, URI: uri, Err: err}
}

func IsUnsupported(err error) bool {
	var be *Error
	return errors.As(err, &be) && be.Kind == ErrorUnsupported
}

func IsTemporary(err error) bool {
	var be *Error
	return errors.As(err, &be) && be.Kind == ErrorTemporary
}

func IsPermanent(err error) bool {
	var be *Error
	return errors.As(err, &be) && be.Kind == ErrorPermanent
}
