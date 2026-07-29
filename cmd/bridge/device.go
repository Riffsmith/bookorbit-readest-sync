package main

import (
	"crypto/rand"
	"fmt"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/config"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/sync/state"
)

// resolveDeviceID determines the stable BookOrbit device identity for this
// bridge instance. Precedence: an explicitly configured device_id, then a
// previously persisted one, then a freshly generated UUID that gets persisted
// in the state store so it survives restarts.
func resolveDeviceID(cfg config.Config, st state.Store) string {
	if cfg.BookOrbit.DeviceID != "" {
		return cfg.BookOrbit.DeviceID
	}
	return st.DeviceID(newUUID)
}

// newUUID returns a random RFC 4122 version 4 UUID string. It panics only if
// the OS entropy source fails, which is unrecoverable for identity generation.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("bridge: cannot read entropy for device id: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
