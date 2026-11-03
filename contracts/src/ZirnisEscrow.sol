// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {IERC20Metadata} from "@openzeppelin/contracts/token/ERC20/extensions/IERC20Metadata.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

/// @notice USDC escrow with explicit buyer, seller and per-agreement arbiter.
/// @dev Escrow funds belong to this contract, not to the ZirnisPayments treasury. Funding
/// escrow must never be treated as a spendable merchant collection. No personal data on-chain.
contract ZirnisEscrow is ReentrancyGuard {
    using SafeERC20 for IERC20;

    enum State { None, Funded, Disputed, Settled }
    struct Escrow {
        address buyer;
        address seller;
        address arbiter;
        uint256 amount;
        uint64 refundAfter;
        State state;
    }

    IERC20 public immutable token;
    mapping(bytes32 => Escrow) public escrows;

    error InvalidAgreement();
    error InvalidState();
    error NotAuthorised();
    error IncorrectAmountReceived();

    event Funded(bytes32 indexed id, bytes32 indexed paymentRef, address indexed buyer, address seller, address arbiter, uint256 amount, uint64 refundAfter);
    event Disputed(bytes32 indexed id, address indexed by);
    event Settled(bytes32 indexed id, uint256 sellerAmount, uint256 buyerAmount);

    constructor(address token_) {
        if (token_.code.length == 0 || IERC20Metadata(token_).decimals() != 6) revert InvalidAgreement();
        token = IERC20(token_);
    }

    function fund(bytes32 paymentRef, address seller, address arbiter, uint256 amount, uint64 refundAfter) external nonReentrant returns (bytes32 id) {
        if (paymentRef == bytes32(0) || seller == address(0) || arbiter == address(0) || seller == msg.sender
            || arbiter == msg.sender || arbiter == seller || seller == address(this) || arbiter == address(this)
            || amount == 0 || amount > uint256(uint64(type(int64).max))
            || refundAfter <= block.timestamp || refundAfter > block.timestamp + 365 days) revert InvalidAgreement();
        // A third party cannot reserve another buyer's paymentRef by front-running funding.
        id = keccak256(abi.encode(msg.sender, paymentRef));
        if (escrows[id].state != State.None) revert InvalidState();
        escrows[id] = Escrow(msg.sender, seller, arbiter, amount, refundAfter, State.Funded);
        uint256 beforeBalance = token.balanceOf(address(this));
        token.safeTransferFrom(msg.sender, address(this), amount);
        if (token.balanceOf(address(this)) - beforeBalance != amount) revert IncorrectAmountReceived();
        emit Funded(id, paymentRef, msg.sender, seller, arbiter, amount, refundAfter);
    }

    function release(bytes32 id) external nonReentrant {
        Escrow storage e = escrows[id];
        if (msg.sender != e.buyer) revert NotAuthorised();
        if (e.state != State.Funded) revert InvalidState();
        _settle(id, e, e.amount);
    }

    function refund(bytes32 id) external nonReentrant {
        Escrow storage e = escrows[id];
        if (msg.sender != e.seller) revert NotAuthorised();
        if (e.state != State.Funded) revert InvalidState();
        _settle(id, e, 0);
    }

    function refundExpired(bytes32 id) external nonReentrant {
        Escrow storage e = escrows[id];
        if (msg.sender != e.buyer) revert NotAuthorised();
        if (e.state != State.Funded || block.timestamp < e.refundAfter) revert InvalidState();
        _settle(id, e, 0);
    }

    function dispute(bytes32 id) external {
        Escrow storage e = escrows[id];
        if (msg.sender != e.buyer && msg.sender != e.seller) revert NotAuthorised();
        if (e.state != State.Funded || block.timestamp >= e.refundAfter) revert InvalidState();
        e.state = State.Disputed;
        emit Disputed(id, msg.sender);
    }

    function resolve(bytes32 id, uint256 sellerAmount) external nonReentrant {
        Escrow storage e = escrows[id];
        if (msg.sender != e.arbiter) revert NotAuthorised();
        if (e.state != State.Disputed || sellerAmount > e.amount) revert InvalidState();
        _settle(id, e, sellerAmount);
    }

    function _settle(bytes32 id, Escrow storage e, uint256 sellerAmount) private {
        e.state = State.Settled;
        uint256 buyerAmount = e.amount - sellerAmount;
        if (sellerAmount != 0) token.safeTransfer(e.seller, sellerAmount);
        if (buyerAmount != 0) token.safeTransfer(e.buyer, buyerAmount);
        emit Settled(id, sellerAmount, buyerAmount);
    }
}
