// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {IERC20Metadata} from "@openzeppelin/contracts/token/ERC20/extensions/IERC20Metadata.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

/// @notice Receives USDC into an immutable treasury and binds each receipt to a Zirnis invoice.
/// @dev paymentRef = bytes16(payment UUID) || uint128(atomic amount), big endian.
/// The contract never holds customer balances, has no upgrade/admin path and cannot debit a
/// wallet without its approval. Treasury payouts and gas funding are separate operations.
contract ZirnisPayments is ReentrancyGuard {
    using SafeERC20 for IERC20;

    struct Payment { address payer; uint256 amount; }

    IERC20 public immutable token;
    address public immutable treasury;
    mapping(bytes32 => Payment) public payments;

    error InvalidConfiguration();
    error InvalidReference();
    error AlreadyPaid();
    error IncorrectAmountReceived();

    event Paid(bytes32 indexed paymentRef, address indexed payer, uint256 amount);

    constructor(address token_, address treasury_) {
        if (token_.code.length == 0 || treasury_ == address(0) || treasury_ == address(this)
            || treasury_ == token_ || IERC20Metadata(token_).decimals() != 6) revert InvalidConfiguration();
        token = IERC20(token_);
        treasury = treasury_;
    }

    function pay(bytes32 paymentRef) external nonReentrant {
        uint256 amount = uint128(uint256(paymentRef));
        if (uint128(uint256(paymentRef) >> 128) == 0 || amount == 0 || amount > uint256(uint64(type(int64).max))) {
            revert InvalidReference();
        }
        if (payments[paymentRef].amount != 0) revert AlreadyPaid();
        payments[paymentRef] = Payment(msg.sender, amount);
        uint256 beforeBalance = token.balanceOf(treasury);
        token.safeTransferFrom(msg.sender, treasury, amount);
        if (token.balanceOf(treasury) - beforeBalance != amount) revert IncorrectAmountReceived();
        emit Paid(paymentRef, msg.sender, amount);
    }
}
