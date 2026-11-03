// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;
import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/// @dev Local tests only. Never use this mintable token as a production settlement asset.
contract MockUSDC is ERC20 {
    constructor() ERC20("Test USD Coin", "USDC") {}
    function decimals() public pure override returns (uint8) { return 6; }
    function mint(address to, uint256 amount) external { _mint(to, amount); }
}

contract FeeToken is ERC20 {
    constructor() ERC20("Fee token", "FEE") {}
    function decimals() public pure override returns (uint8) { return 6; }
    function mint(address to, uint256 amount) external { _mint(to, amount); }
    function _update(address from, address to, uint256 amount) internal override {
        if (from != address(0) && to != address(0)) {
            super._update(from, address(0), amount / 100);
            amount -= amount / 100;
        }
        super._update(from, to, amount);
    }
}
