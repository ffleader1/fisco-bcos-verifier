// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.6.10;

import "./Roles.sol";

/// @title LoanLedger — SellerGraph loan record, repayment ledger, delivery proof and covenant events.
/// @notice The chain is the shared, tamper-evident system of record. It does NOT compute credit decisions:
///         limits, rates and sweep rates come from the ML core / policy engine and are written here once approved.
///         It DOES enforce bookkeeping invariants: funding shares sum to principal, one-time disbursement,
///         human sign-off gates, interest accrual, repayment allocation order (overdue -> interest -> principal),
///         non-decreasing cumulative floor, idempotent repayment references.
/// @dev No PII on chain: sellerRef / payeeRef are salted hashes computed off-chain. Amounts are minor units (fen).
contract LoanLedger is Roles {
    // ---- loan status ----
    uint8 public constant S_NONE = 0;
    uint8 public constant S_CREATED = 1; // waiting for funder confirmations and/or officer sign-off
    uint8 public constant S_APPROVED = 2; // offer can be accepted -> disbursement
    uint8 public constant S_ACTIVE = 3; // disbursed, repaying
    uint8 public constant S_SETTLED = 4;
    uint8 public constant S_DEFAULTED = 5;
    uint8 public constant S_CANCELLED = 6;
    uint8 public constant S_ACCELERATED = 7; // early repayment demanded (covenant breach), officer decision

    // ---- alert levels (Part 11: green / yellow / red) ----
    uint8 public constant A_NONE = 0;
    uint8 public constant A_GREEN = 1;
    uint8 public constant A_YELLOW = 2;
    uint8 public constant A_RED = 3;

    uint256 private constant BPS = 10000;
    uint256 private constant YEAR = 365 days;
    uint256 private constant ACCRUAL_DENOM = BPS * YEAR;

    struct Terms {
        bytes32 sellerRef;
        uint8 tier; // 1 = working capital (order based), 2 = growth capital (campaign based)
        uint256 principal;
        uint256 rateBps; // annual simple interest
        uint256 tenorDays;
        uint256 sweepRateBps; // share of each payout swept to repayment
        uint256 sweepCapBps; // = COGS share of net receipts; sweep rate may never exceed it
        uint256 graceDays;
        bytes32 decisionHash; // hash of ML output + agent memo + challenge + approval package (see AuditTrail)
        bool humanReviewRequired;
        bool officerApproved;
        uint64 createdAt;
    }

    struct State {
        uint8 status;
        uint8 alertLevel;
        uint64 disbursedAt;
        uint64 graceEndsAt;
        uint64 maturityAt;
        uint64 lastAccrualAt;
        uint256 principalOutstanding;
        uint256 interestAccrued; // unpaid interest, includes overdueInterest
        uint256 accrualRemainder; // sub-unit interest carried forward (exact accrual)
        uint256 overduePrincipal; // portion of principalOutstanding that is past due
        uint256 overdueInterest; // portion of interestAccrued that is past due
        uint256 totalRepaid;
        uint256 principalRepaid;
        uint256 interestRepaid;
        uint256 floorCumulative; // cumulative repayment floor ("san luy ke")
        uint256 refundDue; // over-collection owed back to the seller
        uint256 pendingFunders;
    }

    struct Payment {
        bytes32 payeeRef; // supplier reference (hash)
        uint256 amount;
        uint64 deadline; // shipping deadline (check 5)
        bool delivered;
        bytes32 waybillHash;
        uint64 shippedAt;
    }

    // ---- policy (admin) ----
    uint256 public maxRateBps = 2400;
    uint256 public officerThreshold = 50000000; // 500,000.00 CNY in fen; tier-2 loans above need a human signature
    uint256 public maxFutureSkew = 300; // seconds a caller-supplied asOf may run ahead of chain time

    uint256 public totalOutstanding;
    bytes32[] public loanIds;

    mapping(bytes32 => Terms) private terms;
    mapping(bytes32 => State) private state;
    mapping(bytes32 => address[]) private funderList;
    mapping(bytes32 => mapping(address => uint256)) private shareOf;
    mapping(bytes32 => mapping(address => bool)) private confirmedBy;
    mapping(bytes32 => Payment[]) private payments;
    mapping(bytes32 => bool) private usedRef;

    event PolicyUpdated(uint256 maxRateBps, uint256 officerThreshold, uint256 maxFutureSkew);
    event LoanCreated(bytes32 indexed loanId, bytes32 sellerRef, uint8 tier, uint256 principal, bool humanReviewRequired, bytes32 decisionHash);
    event FundingConfirmed(bytes32 indexed loanId, address indexed funder, uint256 amount);
    event OfficerDecision(bytes32 indexed loanId, address indexed officer, bool approved, bytes32 reviewHash);
    event StatusChanged(bytes32 indexed loanId, uint8 fromStatus, uint8 toStatus, bytes32 reasonHash);
    event Disbursed(bytes32 indexed loanId, uint256 amount, bytes32 bankTxRef, uint64 disbursedAt, uint64 graceEndsAt, uint64 maturityAt);
    event PayeePaid(bytes32 indexed loanId, uint256 line, bytes32 payeeRef, uint256 amount, uint64 deadline);
    /// allocation = [toOverdueInterest, toOverduePrincipal, toInterest, toPrincipal, excess]
    event RepaymentRecorded(bytes32 indexed loanId, bytes32 extRef, uint8 source, uint256 amount, uint256[5] allocation, uint64 asOf);
    event FloorCheckpoint(bytes32 indexed loanId, uint256 floorCumulative, uint256 totalRepaid, uint256 shortfall, bool milestone, uint64 asOf);
    event OverdueUpdated(bytes32 indexed loanId, uint256 overduePrincipal, uint256 overdueInterest);
    event AlertRaised(bytes32 indexed loanId, uint8 level, bytes32 code, bytes32 evidenceHash);
    event CovenantEvent(bytes32 indexed loanId, bytes32 code, bool breached, bytes32 evidenceHash);
    event DeliveryRecorded(bytes32 indexed loanId, uint256 line, bytes32 waybillHash, uint64 shippedAt, bool onTime);
    event Restructured(bytes32 indexed loanId, uint64 newMaturityAt, uint256 newSweepRateBps, bytes32 reasonHash);

    // ------------------------------------------------------------------ admin

    function setPolicy(uint256 _maxRateBps, uint256 _officerThreshold, uint256 _maxFutureSkew) external onlyRole(ROLE_ADMIN) {
        require(_maxRateBps > 0 && _maxRateBps <= BPS, "E_INVALID: maxRateBps");
        maxRateBps = _maxRateBps;
        officerThreshold = _officerThreshold;
        maxFutureSkew = _maxFutureSkew;
        emit PolicyUpdated(_maxRateBps, _officerThreshold, _maxFutureSkew);
    }

    // ------------------------------------------------------------------ origination

    /// @param t [tier, principal, rateBps, tenorDays, sweepRateBps, sweepCapBps, graceDays]
    function createLoan(
        bytes32 loanId,
        bytes32 sellerRef,
        uint256[7] calldata t,
        bool humanReviewRequired,
        bytes32 decisionHash,
        address[] calldata funders,
        uint256[] calldata amounts
    ) external onlyRole(ROLE_OPERATOR) {
        require(loanId != bytes32(0), "E_INVALID: loanId");
        require(state[loanId].status == S_NONE, "E_DUPLICATE: loan exists");
        require(sellerRef != bytes32(0), "E_INVALID: sellerRef");
        require(t[0] == 1 || t[0] == 2, "E_INVALID: tier");
        require(t[1] > 0 && t[1] < 2**128, "E_INVALID: principal");
        require(t[2] <= maxRateBps, "E_INVALID: rate above policy ceiling");
        require(t[3] > 0 && t[3] <= 3650, "E_INVALID: tenorDays");
        require(t[5] <= BPS && t[4] <= t[5], "E_INVALID: sweep rate must be <= sweep cap <= 100%");
        require(t[6] < t[3], "E_INVALID: graceDays must be < tenorDays");
        require(decisionHash != bytes32(0), "E_INVALID: decisionHash");
        require(funders.length > 0 && funders.length == amounts.length, "E_INVALID: funding shares");

        Terms storage tm = terms[loanId];
        tm.sellerRef = sellerRef;
        tm.tier = uint8(t[0]);
        tm.principal = t[1];
        tm.rateBps = t[2];
        tm.tenorDays = t[3];
        tm.sweepRateBps = t[4];
        tm.sweepCapBps = t[5];
        tm.graceDays = t[6];
        tm.decisionHash = decisionHash;
        // Tier 2 above the threshold always requires a human signature, whatever the orchestrator says.
        tm.humanReviewRequired = humanReviewRequired || (t[0] == 2 && t[1] > officerThreshold);
        tm.createdAt = uint64(_now());

        State storage s = state[loanId];
        s.status = S_CREATED;
        loanIds.push(loanId);
        emit LoanCreated(loanId, sellerRef, tm.tier, t[1], tm.humanReviewRequired, decisionHash);

        _registerFunders(loanId, funders, amounts, t[1]);
        _maybeApprove(loanId);
    }

    function confirmFunding(bytes32 loanId) external onlyRole(ROLE_FUNDER) {
        State storage s = _loan(loanId);
        require(s.status == S_CREATED, "E_BAD_STATUS: not awaiting funding");
        uint256 amt = shareOf[loanId][msg.sender];
        require(amt > 0, "E_UNAUTHORIZED: not a funder of this loan");
        require(!confirmedBy[loanId][msg.sender], "E_DUPLICATE: already confirmed");
        confirmedBy[loanId][msg.sender] = true;
        s.pendingFunders -= 1;
        emit FundingConfirmed(loanId, msg.sender, amt);
        _maybeApprove(loanId);
    }

    function officerDecision(bytes32 loanId, bool approved, bytes32 reviewHash) external onlyRole(ROLE_OFFICER) {
        State storage s = _loan(loanId);
        require(s.status == S_CREATED, "E_BAD_STATUS: not awaiting sign-off");
        require(reviewHash != bytes32(0), "E_INVALID: reviewHash");
        emit OfficerDecision(loanId, msg.sender, approved, reviewHash);
        if (approved) {
            terms[loanId].officerApproved = true;
            _maybeApprove(loanId);
        } else {
            _setStatus(loanId, s, S_CANCELLED, reviewHash);
        }
    }

    /// @notice Offer declined / expired before disbursement.
    function cancelLoan(bytes32 loanId, bytes32 reasonHash) external onlyRole(ROLE_OPERATOR) {
        State storage s = _loan(loanId);
        require(s.status == S_CREATED || s.status == S_APPROVED, "E_BAD_STATUS: cannot cancel after disbursement");
        _setStatus(loanId, s, S_CANCELLED, reasonHash);
    }

    // ------------------------------------------------------------------ disbursement ("one click, one payment")

    function recordDisbursement(
        bytes32 loanId,
        bytes32[] calldata payeeRefs,
        uint256[] calldata amounts,
        uint256 deliveryDays,
        bytes32 bankTxRef
    ) external onlyRole(ROLE_OPERATOR) {
        State storage s = _loan(loanId);
        require(s.status == S_APPROVED, "E_BAD_STATUS: loan not approved");
        require(payeeRefs.length > 0 && payeeRefs.length == amounts.length, "E_INVALID: payment lines");
        require(bankTxRef != bytes32(0), "E_INVALID: bankTxRef");
        require(deliveryDays > 0 && deliveryDays <= 365, "E_INVALID: deliveryDays");
        Terms storage tm = terms[loanId];

        uint256 nowTs = _now();
        uint64 deadline = uint64(nowTs + deliveryDays * 1 days);
        uint256 sum = 0;
        for (uint256 i = 0; i < payeeRefs.length; i++) {
            require(payeeRefs[i] != bytes32(0) && amounts[i] > 0 && amounts[i] <= tm.principal, "E_INVALID: payment line");
            payments[loanId].push(Payment(payeeRefs[i], amounts[i], deadline, false, bytes32(0), 0));
            sum += amounts[i];
            emit PayeePaid(loanId, i, payeeRefs[i], amounts[i], deadline);
        }
        require(sum == tm.principal, "E_INVALID: payments must sum to principal (single, full disbursement)");

        s.disbursedAt = uint64(nowTs);
        s.lastAccrualAt = uint64(nowTs);
        s.graceEndsAt = uint64(nowTs + tm.graceDays * 1 days);
        s.maturityAt = uint64(nowTs + tm.tenorDays * 1 days);
        s.principalOutstanding = tm.principal;
        totalOutstanding += tm.principal;
        emit Disbursed(loanId, tm.principal, bankTxRef, s.disbursedAt, s.graceEndsAt, s.maturityAt);
        _setStatus(loanId, s, S_ACTIVE, bankTxRef);
    }

    // ------------------------------------------------------------------ repayment ledger

    /// @param source 1 WeBank sweep, 2 authorized-account debit, 3 proportional payout sweep,
    ///               4 floor make-up debit, 5 voluntary prepayment, 6 recovery, 7 other
    function recordRepayment(bytes32 loanId, uint256 amount, uint8 source, bytes32 extRef, uint256 asOf)
        external
        onlyRole(ROLE_OPERATOR)
    {
        State storage s = _loan(loanId);
        require(s.status == S_ACTIVE || s.status == S_ACCELERATED || s.status == S_DEFAULTED, "E_BAD_STATUS: loan not repayable");
        require(amount > 0 && amount < 2**128, "E_INVALID: amount");
        require(source >= 1 && source <= 7, "E_INVALID: source");
        require(extRef != bytes32(0), "E_INVALID: extRef");
        bytes32 refKey = keccak256(abi.encodePacked(loanId, extRef));
        require(!usedRef[refKey], "E_DUPLICATE: repayment reference already recorded");
        usedRef[refKey] = true;
        _accrue(s, terms[loanId].rateBps, asOf);

        uint256[5] memory a;
        uint256 rest = amount;
        a[0] = _min(rest, s.overdueInterest);
        rest -= a[0];
        a[1] = _min(rest, s.overduePrincipal);
        rest -= a[1];
        a[2] = _min(rest, s.interestAccrued - a[0]);
        rest -= a[2];
        a[3] = _min(rest, s.principalOutstanding - a[1]);
        rest -= a[3];
        a[4] = rest;

        s.overdueInterest -= a[0];
        s.overduePrincipal -= a[1];
        s.interestAccrued -= (a[0] + a[2]);
        s.principalOutstanding -= (a[1] + a[3]);
        s.interestRepaid += (a[0] + a[2]);
        s.principalRepaid += (a[1] + a[3]);
        s.totalRepaid += (amount - a[4]);
        s.refundDue += a[4];
        totalOutstanding -= (a[1] + a[3]);

        emit RepaymentRecorded(loanId, extRef, source, amount, a, uint64(asOf));
        if (a[0] + a[1] > 0) emit OverdueUpdated(loanId, s.overduePrincipal, s.overdueInterest);

        if (s.principalOutstanding == 0 && s.interestAccrued == 0) {
            s.accrualRemainder = 0;
            s.alertLevel = A_NONE;
            _setStatus(loanId, s, S_SETTLED, extRef);
        }
    }

    /// @notice Cumulative-floor check. Below floor -> YELLOW alert (make-up debit expected).
    ///         At a milestone, any remaining shortfall becomes overdue.
    function checkpointFloor(bytes32 loanId, uint256 floorCumulative, uint256 asOf, bool milestone)
        external
        onlyRole(ROLE_OPERATOR | ROLE_MONITOR)
    {
        State storage s = _loan(loanId);
        require(s.status == S_ACTIVE || s.status == S_ACCELERATED, "E_BAD_STATUS: loan not active");
        require(floorCumulative >= s.floorCumulative, "E_INVALID: floor cannot decrease");
        _accrue(s, terms[loanId].rateBps, asOf);
        s.floorCumulative = floorCumulative;
        uint256 shortfall = floorCumulative > s.totalRepaid ? floorCumulative - s.totalRepaid : 0;
        emit FloorCheckpoint(loanId, floorCumulative, s.totalRepaid, shortfall, milestone, uint64(asOf));
        if (shortfall > 0) {
            _raise(loanId, s, A_YELLOW, "FLOOR_SHORTFALL", bytes32(shortfall));
            if (milestone) _setOverdueAtLeast(loanId, s, shortfall);
        }
    }

    /// @notice After maturity everything still owed is overdue; RED alert to a human officer.
    function markMatured(bytes32 loanId, uint256 asOf) external onlyRole(ROLE_OPERATOR | ROLE_MONITOR) {
        State storage s = _loan(loanId);
        require(s.status == S_ACTIVE || s.status == S_ACCELERATED, "E_BAD_STATUS: loan not active");
        require(asOf >= s.maturityAt, "E_INVALID: not matured yet");
        _accrue(s, terms[loanId].rateBps, asOf);
        _setOverdueAtLeast(loanId, s, s.principalOutstanding + s.interestAccrued);
        _raise(loanId, s, A_RED, "MATURED_UNPAID", bytes32(0));
    }

    /// @notice Covenant breach -> early repayment demand. Human officer only (Agent 5 may not do this).
    function accelerate(bytes32 loanId, bytes32 reasonHash, uint256 asOf) external onlyRole(ROLE_OFFICER) {
        State storage s = _loan(loanId);
        require(s.status == S_ACTIVE, "E_BAD_STATUS: loan not active");
        require(reasonHash != bytes32(0), "E_INVALID: reasonHash");
        _accrue(s, terms[loanId].rateBps, asOf);
        _setOverdueAtLeast(loanId, s, s.principalOutstanding + s.interestAccrued);
        _raise(loanId, s, A_RED, "ACCELERATED", reasonHash);
        _setStatus(loanId, s, S_ACCELERATED, reasonHash);
    }

    function markDefault(bytes32 loanId, bytes32 reasonHash) external onlyRole(ROLE_OFFICER) {
        State storage s = _loan(loanId);
        require(s.status == S_ACTIVE || s.status == S_ACCELERATED, "E_BAD_STATUS: loan not active");
        require(reasonHash != bytes32(0), "E_INVALID: reasonHash");
        _setStatus(loanId, s, S_DEFAULTED, reasonHash);
    }

    /// @notice Proactive restructuring ("gian no") approved by a human officer: extend maturity, new sweep rate,
    ///         overdue buckets re-scheduled, floor restarts from what has been repaid.
    function restructure(bytes32 loanId, uint256 extraDays, uint256 newSweepRateBps, bytes32 reasonHash, uint256 asOf)
        external
        onlyRole(ROLE_OFFICER)
    {
        State storage s = _loan(loanId);
        Terms storage tm = terms[loanId];
        require(s.status == S_ACTIVE || s.status == S_ACCELERATED, "E_BAD_STATUS: loan not active");
        require(reasonHash != bytes32(0), "E_INVALID: reasonHash");
        require(extraDays <= 365, "E_INVALID: extraDays");
        require(newSweepRateBps <= tm.sweepCapBps, "E_INVALID: sweep rate above cap");
        _accrue(s, tm.rateBps, asOf);
        s.maturityAt = uint64(uint256(s.maturityAt) + extraDays * 1 days);
        tm.tenorDays += extraDays;
        tm.sweepRateBps = newSweepRateBps;
        s.overduePrincipal = 0;
        s.overdueInterest = 0;
        s.floorCumulative = s.totalRepaid;
        s.alertLevel = A_YELLOW;
        emit OverdueUpdated(loanId, 0, 0);
        emit Restructured(loanId, s.maturityAt, newSweepRateBps, reasonHash);
        if (s.status != S_ACTIVE) _setStatus(loanId, s, S_ACTIVE, reasonHash);
    }

    // ------------------------------------------------------------------ monitoring, delivery proof, covenants

    function raiseAlert(bytes32 loanId, uint8 level, bytes32 code, bytes32 evidenceHash) external onlyRole(ROLE_MONITOR) {
        State storage s = _loan(loanId);
        require(level >= A_GREEN && level <= A_RED, "E_INVALID: level");
        require(code != bytes32(0), "E_INVALID: code");
        s.alertLevel = level;
        emit AlertRaised(loanId, level, code, evidenceHash);
    }

    function recordCovenantEvent(bytes32 loanId, bytes32 code, bool breached, bytes32 evidenceHash) external onlyRole(ROLE_MONITOR) {
        State storage s = _loan(loanId);
        require(s.status == S_ACTIVE || s.status == S_ACCELERATED, "E_BAD_STATUS: loan not active");
        require(code != bytes32(0), "E_INVALID: code");
        emit CovenantEvent(loanId, code, breached, evidenceHash);
    }

    /// @notice Check 5: goods actually shipped within the committed number of days after payment.
    function recordDelivery(bytes32 loanId, uint256 line, bytes32 waybillHash, uint256 shippedAt)
        external
        onlyRole(ROLE_OPERATOR | ROLE_MONITOR)
    {
        State storage s = _loan(loanId);
        require(s.status >= S_ACTIVE && s.status != S_CANCELLED, "E_BAD_STATUS: not disbursed");
        require(line < payments[loanId].length, "E_NOT_FOUND: payment line");
        require(waybillHash != bytes32(0), "E_INVALID: waybillHash");
        Payment storage p = payments[loanId][line];
        require(!p.delivered, "E_DUPLICATE: delivery already recorded");
        require(shippedAt >= s.disbursedAt && shippedAt <= _now() + maxFutureSkew, "E_INVALID: shippedAt");
        p.delivered = true;
        p.waybillHash = waybillHash;
        p.shippedAt = uint64(shippedAt);
        bool onTime = shippedAt <= p.deadline;
        emit DeliveryRecorded(loanId, line, waybillHash, uint64(shippedAt), onTime);
        if (!onTime) emit CovenantEvent(loanId, "DELIVERY_LATE", true, waybillHash);
    }

    function flagMissingDelivery(bytes32 loanId, uint256 line, uint256 asOf) external onlyRole(ROLE_MONITOR) {
        State storage s = _loan(loanId);
        require(s.status == S_ACTIVE || s.status == S_ACCELERATED, "E_BAD_STATUS: loan not active");
        require(line < payments[loanId].length, "E_NOT_FOUND: payment line");
        require(asOf <= _now() + maxFutureSkew, "E_INVALID: asOf in the future");
        Payment storage p = payments[loanId][line];
        require(!p.delivered, "E_INVALID: already delivered");
        require(asOf > p.deadline, "E_INVALID: deadline not passed");
        emit CovenantEvent(loanId, "DELIVERY_MISSING", true, p.payeeRef);
        _raise(loanId, s, A_RED, "DELIVERY_MISSING", p.payeeRef);
    }

    // ------------------------------------------------------------------ views

    function loanCount() external view returns (uint256) {
        return loanIds.length;
    }

    function getTerms(bytes32 loanId)
        external
        view
        returns (
            bytes32 sellerRef,
            uint8 tier,
            uint256 principal,
            uint256 rateBps,
            uint256 tenorDays,
            uint256 sweepRateBps,
            uint256 sweepCapBps,
            uint256 graceDays
        )
    {
        Terms storage t = terms[loanId];
        return (t.sellerRef, t.tier, t.principal, t.rateBps, t.tenorDays, t.sweepRateBps, t.sweepCapBps, t.graceDays);
    }

    function getFlags(bytes32 loanId)
        external
        view
        returns (bytes32 decisionHash, bool humanReviewRequired, bool officerApproved, uint64 createdAt, uint256 pendingFunders)
    {
        Terms storage t = terms[loanId];
        return (t.decisionHash, t.humanReviewRequired, t.officerApproved, t.createdAt, state[loanId].pendingFunders);
    }

    function getSchedule(bytes32 loanId)
        external
        view
        returns (uint8 status, uint8 alertLevel, uint64 disbursedAt, uint64 graceEndsAt, uint64 maturityAt, uint64 lastAccrualAt)
    {
        State storage s = state[loanId];
        return (s.status, s.alertLevel, s.disbursedAt, s.graceEndsAt, s.maturityAt, s.lastAccrualAt);
    }

    function getBalances(bytes32 loanId)
        external
        view
        returns (
            uint256 principalOutstanding,
            uint256 interestAccrued,
            uint256 overduePrincipal,
            uint256 overdueInterest,
            uint256 totalRepaid,
            uint256 principalRepaid,
            uint256 interestRepaid,
            uint256 floorCumulative,
            uint256 refundDue
        )
    {
        State storage s = state[loanId];
        return (
            s.principalOutstanding,
            s.interestAccrued,
            s.overduePrincipal,
            s.overdueInterest,
            s.totalRepaid,
            s.principalRepaid,
            s.interestRepaid,
            s.floorCumulative,
            s.refundDue
        );
    }

    function getFunding(bytes32 loanId) external view returns (address[] memory funders, uint256[] memory amounts, bool[] memory confirmed) {
        address[] storage fl = funderList[loanId];
        funders = new address[](fl.length);
        amounts = new uint256[](fl.length);
        confirmed = new bool[](fl.length);
        for (uint256 i = 0; i < fl.length; i++) {
            funders[i] = fl[i];
            amounts[i] = shareOf[loanId][fl[i]];
            confirmed[i] = confirmedBy[loanId][fl[i]];
        }
    }

    function paymentCount(bytes32 loanId) external view returns (uint256) {
        return payments[loanId].length;
    }

    function getPayment(bytes32 loanId, uint256 line)
        external
        view
        returns (bytes32 payeeRef, uint256 amount, uint64 deadline, bool delivered, bytes32 waybillHash, uint64 shippedAt)
    {
        require(line < payments[loanId].length, "E_NOT_FOUND: payment line");
        Payment storage p = payments[loanId][line];
        return (p.payeeRef, p.amount, p.deadline, p.delivered, p.waybillHash, p.shippedAt);
    }

    /// @notice Payoff quote as of a given time (no state change). Prepayment carries no fee.
    function previewPayoff(bytes32 loanId, uint256 asOf) external view returns (uint256 principal, uint256 interest, uint256 total) {
        State storage s = state[loanId];
        require(s.status != S_NONE, "E_NOT_FOUND: loan");
        principal = s.principalOutstanding;
        interest = s.interestAccrued;
        if ((s.status == S_ACTIVE || s.status == S_ACCELERATED || s.status == S_DEFAULTED) && asOf > s.lastAccrualAt) {
            uint256 num = principal * terms[loanId].rateBps * (asOf - s.lastAccrualAt) + s.accrualRemainder;
            interest += num / ACCRUAL_DENOM;
        }
        total = principal + interest;
    }

    // ------------------------------------------------------------------ internals

    function _loan(bytes32 loanId) internal view returns (State storage s) {
        s = state[loanId];
        require(s.status != S_NONE, "E_NOT_FOUND: loan");
    }

    function _registerFunders(bytes32 loanId, address[] calldata funders, uint256[] calldata amounts, uint256 principal) internal {
        State storage s = state[loanId];
        uint256 sum = 0;
        for (uint256 i = 0; i < funders.length; i++) {
            address f = funders[i];
            require(hasRole(f, ROLE_FUNDER), "E_INVALID: not a registered funder");
            require(amounts[i] > 0 && amounts[i] <= principal, "E_INVALID: share amount");
            require(shareOf[loanId][f] == 0, "E_DUPLICATE: funder listed twice");
            shareOf[loanId][f] = amounts[i];
            funderList[loanId].push(f);
            sum += amounts[i];
            if (f == msg.sender) {
                confirmedBy[loanId][f] = true; // operator key that is also the funder key (WeBank-only mode)
                emit FundingConfirmed(loanId, f, amounts[i]);
            } else {
                s.pendingFunders += 1;
            }
        }
        require(sum == principal, "E_INVALID: funding shares must sum to principal");
    }

    function _maybeApprove(bytes32 loanId) internal {
        State storage s = state[loanId];
        Terms storage t = terms[loanId];
        if (s.status == S_CREATED && s.pendingFunders == 0 && (!t.humanReviewRequired || t.officerApproved)) {
            _setStatus(loanId, s, S_APPROVED, t.decisionHash);
        }
    }

    function _setStatus(bytes32 loanId, State storage s, uint8 to, bytes32 reasonHash) internal {
        uint8 from = s.status;
        s.status = to;
        emit StatusChanged(loanId, from, to, reasonHash);
    }

    function _raise(bytes32 loanId, State storage s, uint8 level, bytes32 code, bytes32 evidence) internal {
        if (level > s.alertLevel) s.alertLevel = level;
        emit AlertRaised(loanId, level, code, evidence);
    }

    /// @dev Simple daily interest on outstanding principal, exact to the second with carried remainder.
    function _accrue(State storage s, uint256 rateBps, uint256 asOf) internal {
        require(asOf >= s.lastAccrualAt, "E_INVALID: asOf before last accrual");
        require(asOf <= _now() + maxFutureSkew, "E_INVALID: asOf in the future");
        if (asOf == s.lastAccrualAt) return;
        uint256 num = s.principalOutstanding * rateBps * (asOf - s.lastAccrualAt) + s.accrualRemainder;
        s.interestAccrued += num / ACCRUAL_DENOM;
        s.accrualRemainder = num % ACCRUAL_DENOM;
        s.lastAccrualAt = uint64(asOf);
    }

    /// @dev Raise the overdue total to at least `due` (capped by what is owed). Interest is classified first.
    function _setOverdueAtLeast(bytes32 loanId, State storage s, uint256 due) internal {
        uint256 owed = s.principalOutstanding + s.interestAccrued;
        if (due > owed) due = owed;
        if (due <= s.overduePrincipal + s.overdueInterest) return;
        uint256 oi = _min(due, s.interestAccrued);
        s.overdueInterest = oi;
        s.overduePrincipal = due - oi;
        emit OverdueUpdated(loanId, s.overduePrincipal, s.overdueInterest);
    }

    function _min(uint256 a, uint256 b) internal pure returns (uint256) {
        return a < b ? a : b;
    }
}
