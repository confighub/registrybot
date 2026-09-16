// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

// version is stamped at build time with -ldflags "-X main.version=vX.Y.Z".
// It is reported on /healthz and in the User-Agent sent to ConfigHub and GitHub.
var version = "dev"
