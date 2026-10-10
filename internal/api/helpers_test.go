package api_test

import "buoyup/fisco/internal/sealing"

func sealingNewKeyPair() (sealing.KeyPair, error) { return sealing.NewKeyPair() }
