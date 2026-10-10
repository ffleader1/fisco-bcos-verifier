// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.6.10;

import "./Roles.sol";

/// @title AuditTrail — hash anchoring for algorithm outputs, agent I/O, proposals, control reviews, human decisions
///        and explanations (used by Agents 1, 2, 3 and 5 and the approval gate).
/// @notice Only hashes go on chain; documents stay in WeBank storage. Each subject (application / loan id) keeps a
///         hash chain: head_n = keccak256(head_{n-1} || kind || contentHash || timestamp), so deleting or reordering
///         any off-chain record is detectable.
contract AuditTrail is Roles {
    // kinds
    uint8 public constant K_APPLICATION = 1; // submitted file set / extracted fields
    uint8 public constant K_ALGORITHM_OUTPUT = 2; // algorithm core ticket: limits, rate, grade, sweep rate, reasons
    uint8 public constant K_AGENT_IO = 3; // full prompt/response trace of an agent step
    uint8 public constant K_CREDIT_MEMO = 4; // Agent 2 proposal
    uint8 public constant K_RISK_CHALLENGE = 5; // Agent 3 control review (phase 1 seal and final conclusion)
    uint8 public constant K_HUMAN_DECISION = 6; // WeBank officer decision
    uint8 public constant K_OFFER_CONTRACT = 7; // offer + loan contract accepted by the seller
    uint8 public constant K_EXPLANATION = 8; // Art. 24 PIPL explanation sent to the seller
    uint8 public constant K_SWEEP_NOTICE = 9; // per-debit explanation notice
    uint8 public constant K_DECISION_PACKAGE = 10; // full approval package (approval gate output)
    uint8 public constant K_OTHER = 11;

    struct Record {
        uint8 kind;
        bytes32 contentHash;
        uint64 timestamp;
        address writer;
        bytes32 head;
    }

    mapping(bytes32 => Record[]) private records;
    mapping(bytes32 => bytes32) public headOf;
    mapping(bytes32 => bool) private seen; // keccak(subject, contentHash)
    mapping(bytes32 => bytes32) private firstSubject; // contentHash -> subject
    mapping(bytes32 => uint256) private firstIndexPlus1; // contentHash -> index + 1
    uint256 public totalRecords;

    event Anchored(bytes32 indexed subject, uint256 indexed index, uint8 kind, bytes32 contentHash, bytes32 head, uint64 timestamp, string uri);

    function anchor(bytes32 subject, uint8 kind, bytes32 contentHash, string calldata uri)
        external
        onlyRole(ROLE_AUDIT_WRITER)
        returns (uint256 index, bytes32 head)
    {
        require(subject != bytes32(0), "E_INVALID: subject");
        require(kind >= K_APPLICATION && kind <= K_OTHER, "E_INVALID: kind");
        require(contentHash != bytes32(0), "E_INVALID: contentHash");
        require(bytes(uri).length <= 256, "E_INVALID: uri too long");
        bytes32 key = keccak256(abi.encodePacked(subject, contentHash));
        require(!seen[key], "E_DUPLICATE: already anchored for this subject");
        seen[key] = true;

        uint64 ts = uint64(_now());
        head = keccak256(abi.encodePacked(headOf[subject], kind, contentHash, ts));
        index = records[subject].length;
        records[subject].push(Record(kind, contentHash, ts, msg.sender, head));
        headOf[subject] = head;
        totalRecords += 1;
        if (firstIndexPlus1[contentHash] == 0) {
            firstSubject[contentHash] = subject;
            firstIndexPlus1[contentHash] = index + 1;
        }
        emit Anchored(subject, index, kind, contentHash, head, ts, uri);
    }

    function recordCount(bytes32 subject) external view returns (uint256) {
        return records[subject].length;
    }

    function getRecord(bytes32 subject, uint256 index)
        external
        view
        returns (uint8 kind, bytes32 contentHash, uint64 timestamp, address writer, bytes32 head)
    {
        require(index < records[subject].length, "E_NOT_FOUND: record");
        Record storage r = records[subject][index];
        return (r.kind, r.contentHash, r.timestamp, r.writer, r.head);
    }

    /// @notice Where was this content hash first anchored?
    function locate(bytes32 contentHash) external view returns (bool found, bytes32 subject, uint256 index) {
        uint256 ip1 = firstIndexPlus1[contentHash];
        if (ip1 == 0) return (false, bytes32(0), 0);
        return (true, firstSubject[contentHash], ip1 - 1);
    }
}
