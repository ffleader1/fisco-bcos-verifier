// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.6.10;

/// @title Roles — minimal bitmask role registry shared by SellerGraph contracts.
/// @notice Role separation mirrors the SellerGraph operating model:
///         - OPERATOR : the fixed-code orchestrator (backend). Never an AI agent acting on its own authority.
///         - OFFICER  : a human WeBank credit officer (sign-off above threshold, acceleration, default, restructuring).
///         - MONITOR  : post-disbursement monitoring service (Agent 5 pipeline) — can raise alerts, cannot accelerate.
///         - FUNDER   : a lender account that must confirm its funding share (WeBank by default; N funders supported).
///         - AUDIT_WRITER : may anchor hashes of ML / agent / approval artefacts.
/// @dev Revert strings start with a stable code (E_UNAUTHORIZED, E_NOT_FOUND, E_BAD_STATUS, E_INVALID, E_DUPLICATE)
///      that the Go service maps to HTTP status codes.
contract Roles {
    uint256 public constant ROLE_ADMIN = 1;
    uint256 public constant ROLE_OPERATOR = 2;
    uint256 public constant ROLE_OFFICER = 4;
    uint256 public constant ROLE_MONITOR = 8;
    uint256 public constant ROLE_FUNDER = 16;
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
