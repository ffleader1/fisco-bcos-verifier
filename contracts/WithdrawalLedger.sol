// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.6.10;

import "./Roles.sol";

/// @title WithdrawalLedger — tamper-proof, confidential record of every withdrawal BouyUp pays out.
/// @notice BouyUp pays the target account first, then claims each bank's share back from that bank. A record
///         proves who funded what without showing it to everybody:
///         - `envelope`: the withdrawal encrypted in pieces (summary + one piece per bank). Each bank can decrypt
///           only the summary and its own share; the BouyUp master key decrypts everything. The contract never
///           reads it (see internal/sealing for the format).
///         - `commitments`: SHA-256 of each piece's plaintext (salted), so a decrypted piece can be proven
///           unchanged.
///         - `root`: Merkle root of the commitments, recomputed here: a record whose root does not match is
///           rejected.
///         The 100.00% bank-split rule is enforced by the service before encryption and can be re-checked by
///         anyone holding the master key; banks only see their own share.
contract WithdrawalLedger is Roles {
    uint256 public constant MIN_PIECES = 2; // summary + at least one bank
    uint256 public constant MAX_PIECES = 21; // summary + up to 20 banks
    uint256 public constant MAX_ENVELOPE_BYTES = 32768;

    struct Withdrawal {
        bytes32 root;
        bytes32[] commitments;
        bytes envelope;
        uint64 timestamp;
        uint64 blockNumber;
        address writer;
    }

    mapping(bytes32 => Withdrawal) private withdrawals;
    mapping(bytes32 => bool) public exists;
    bytes32[] public withdrawalIds;

    event WithdrawalRecorded(bytes32 indexed withdrawalId, bytes32 root, uint256 pieces, uint256 envelopeBytes, uint64 timestamp);

    function record(bytes32 withdrawalId, bytes32 root, bytes32[] calldata commitments, bytes calldata envelope)
        external
        onlyRole(ROLE_OPERATOR)
    {
        require(withdrawalId != bytes32(0), "E_INVALID: withdrawalId");
        require(!exists[withdrawalId], "E_DUPLICATE: withdrawal already recorded");
        require(commitments.length >= MIN_PIECES && commitments.length <= MAX_PIECES, "E_INVALID: 2-21 pieces");
        require(envelope.length > 0 && envelope.length <= MAX_ENVELOPE_BYTES, "E_INVALID: envelope size");
        for (uint256 i = 0; i < commitments.length; i++) {
            require(commitments[i] != bytes32(0), "E_INVALID: empty commitment");
        }
        require(root == merkleRoot(commitments), "E_INVALID: root does not match the commitments");

        Withdrawal storage w = withdrawals[withdrawalId];
        w.root = root;
        w.commitments = commitments;
        w.envelope = envelope;
        w.timestamp = uint64(_now());
        w.blockNumber = uint64(block.number);
        w.writer = msg.sender;
        exists[withdrawalId] = true;
        withdrawalIds.push(withdrawalId);
        emit WithdrawalRecorded(withdrawalId, root, commitments.length, envelope.length, w.timestamp);
    }

    /// @notice SHA-256 Merkle root: pairs hashed as sha256(left || right); an odd last node moves up unchanged.
    function merkleRoot(bytes32[] memory leaves) public pure returns (bytes32) {
        uint256 n = leaves.length;
        if (n == 0) return bytes32(0);
        bytes32[] memory level = leaves;
        while (n > 1) {
            uint256 m = (n + 1) / 2;
            bytes32[] memory next = new bytes32[](m);
            for (uint256 i = 0; i < n / 2; i++) {
                next[i] = sha256(abi.encodePacked(level[2 * i], level[2 * i + 1]));
            }
            if (n % 2 == 1) next[m - 1] = level[n - 1];
            level = next;
            n = m;
        }
        return level[0];
    }

    function withdrawalCount() external view returns (uint256) {
        return withdrawalIds.length;
    }

    function getWithdrawal(bytes32 withdrawalId)
        external
        view
        returns (bytes32 root, bytes32[] memory commitments, bytes memory envelope, uint64 timestamp, uint64 blockNumber, address writer)
    {
        require(exists[withdrawalId], "E_NOT_FOUND: withdrawal");
        Withdrawal storage w = withdrawals[withdrawalId];
        return (w.root, w.commitments, w.envelope, w.timestamp, w.blockNumber, w.writer);
    }
}
