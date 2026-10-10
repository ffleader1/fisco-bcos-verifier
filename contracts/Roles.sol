// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.6.10;

/// @title Roles — minimal bitmask role registry shared by BouyUp contracts.
/// @notice - ADMIN        : deploys the contracts and grants roles.
///         - OPERATOR     : the BouyUp service key. Writes withdrawal records (WithdrawalLedger).
///         - AUDIT_WRITER : may anchor hashes of agent / algorithm / human artefacts (AuditTrail).
///         Who actually triggered a withdrawal (Agent 4 or a human) is recorded in the withdrawal itself;
///         the chain only accepts writes signed by the service.
/// @dev Revert strings start with a stable code (E_UNAUTHORIZED, E_NOT_FOUND, E_INVALID, E_DUPLICATE)
///      that the Go service maps to HTTP status codes. Bit values are kept stable across versions.
contract Roles {
    uint256 public constant ROLE_ADMIN = 1;
    uint256 public constant ROLE_OPERATOR = 2;
    uint256 public constant ROLE_AUDIT_WRITER = 32;

    mapping(address => uint256) public roles;

    event RoleGranted(address indexed account, uint256 role);
    event RoleRevoked(address indexed account, uint256 role);

    constructor() public {
        roles[msg.sender] = ROLE_ADMIN;
        emit RoleGranted(msg.sender, ROLE_ADMIN);
    }

    modifier onlyRole(uint256 role) {
        require(roles[msg.sender] & role != 0, "E_UNAUTHORIZED: missing role");
        _;
    }

    function hasRole(address account, uint256 role) public view returns (bool) {
        return roles[account] & role != 0;
    }

    function grantRole(address account, uint256 role) external onlyRole(ROLE_ADMIN) {
        require(account != address(0), "E_INVALID: zero address");
        roles[account] = roles[account] | role;
        emit RoleGranted(account, role);
    }

    function revokeRole(address account, uint256 role) external onlyRole(ROLE_ADMIN) {
        require(!(account == msg.sender && (role & ROLE_ADMIN) != 0), "E_INVALID: cannot revoke own admin");
        roles[account] = roles[account] & ~role;
        emit RoleRevoked(account, role);
    }

    /// @dev FISCO BCOS 2.x block.timestamp is in MILLISECONDS. All contract times are unix seconds.
    function _now() internal view returns (uint256) {
        return block.timestamp / 1000;
    }
}
