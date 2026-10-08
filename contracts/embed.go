// Package contracts embeds the compiled SellerGraph Solidity contracts (solc 0.6.10, optimizer 200 runs,
// evmVersion constantinople). Rebuild with `make contracts` after editing the .sol files.
package contracts

import _ "embed"

//go:embed build/LoanLedger.abi
var LoanLedgerABI string

//go:embed build/LoanLedger.bin
var LoanLedgerBin string

//go:embed build/AuditTrail.abi
var AuditTrailABI string

//go:embed build/AuditTrail.bin
var AuditTrailBin string
