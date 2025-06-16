// SPDX-License-Identifier: MIT

pragma solidity ^0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/**
 * @title RebaseToken
 * @author Ishan Suryawanshi
 * @notice This is a cross chain token than incentivises users to deposit into a vault and gain interest in rewards.
 * @notice The interest in the contract can only decrease
 * @notice Each user will have their own interest rate which is the global interest rate at the time of depositing
 */

contract RebaseToken is ERC20 {
    error RebaseToken__InterestRateCanOnlyDecrease(uint256 oldInterestRate, uint256 newInterestRate);

    uint256 private s_interestRate = 5e10;
    mapping(address=>uint256) private s_userInterestRate;

    event InterestRateSet(uint256 newInterestRate);

    constructor() ERC20("Rebase Token", "RBT"){}

    /**
     * @notice Set the interest rate in the contract 
     * @param _newInterestRate: New interest rate to set
     * @dev The interest rate can only decrease
     */
    function setInterestRate(uint256 _newInterestRate) external {
        if(_newInterestRate > s_interestRate){
            revert RebaseToken__InterestRateCanOnlyDecrease(s_interestRate, _newInterestRate);
        }
        s_interestRate = _newInterestRate;
        emit InterestRateSet(_newInterestRate);
    }

    function mint(address _to, uint256 _amount) external {
        s_userInterestRate[_to] = s_interestRate;
        _mint(_to, _amount);
    }

    function _mintAcruedInterest(address _user) internal {
        // find the current balance of rebase tokens that have been minted to the user -> principal balance
        // calculate their current balance including any interest -> balanceOf
        // mint the difference to the user
    }

    /**
     * @notice Get interest rate for the user
     * @param _user: The user to get the interest rate for 
     * @return The interest rate of the user
     */
    function getUseInterestRate(address _user) external view returns(uint256){
        return s_userInterestRate[_user];
    }
}