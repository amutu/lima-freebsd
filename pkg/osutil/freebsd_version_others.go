// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !freebsd

package osutil

import "errors"

// FreeBSDVersion is only meaningful on FreeBSD.
func FreeBSDVersion() (string, error) {
	return "", errors.New("FreeBSD version is only available on FreeBSD")
}
