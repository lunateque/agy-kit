//go:build !windows

package auth

import "errors"

func readSysCredential(target string) (*RawCredential, error) {
	return nil, errors.New("wincred credential store is only supported on Windows")
}

func writeSysCredential(raw *RawCredential) error {
	return errors.New("wincred credential store is only supported on Windows")
}
