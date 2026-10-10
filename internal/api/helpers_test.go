package api_test

import "bouyup/fisco/internal/sealing"

func sealingNewKeyPair() (sealing.KeyPair, error) { return sealing.NewKeyPair() }
