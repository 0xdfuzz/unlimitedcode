//go:build !windows

package setup

import "errors"

var errUnsupported = errors.New("this action is only implemented on Windows")

func UserPath() (string, error) { return "", errUnsupported }

func SetUserPath(string) error { return errUnsupported }

func TakeoverPlan() ([]string, string, error) { return nil, "", errUnsupported }

func TakeoverPath(string) ([]string, []string, bool, error) { return nil, nil, false, errUnsupported }

func Restore() (bool, bool, error) { return false, false, errUnsupported }
