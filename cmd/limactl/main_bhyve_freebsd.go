//go:build !external_bhyve

// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Import the bhyve driver to register it in the registry on FreeBSD.
import _ "github.com/lima-vm/lima/v2/pkg/driver/bhyve"
