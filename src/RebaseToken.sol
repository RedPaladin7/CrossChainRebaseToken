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
    uint256 private constant PRECISION_FACTOR = 1e18;
    mapping(address=>uint256) private s_userInterestRate;
    mapping(address=>uint256) private s_userLastUpdatedTimestamp;

    event InterestRateSet(uint256 newInterestRate);

    constructor() ERC20("Rebase Token", "RBT"){}

    /**
     * @notice Set the interest rate in the contract 
     * @param _newInterestRate New interest rate to set
     * @dev The interest rate can only decrease
     */
    function setInterestRate(uint256 _newInterestRate) external {
        if(_newInterestRate > s_interestRate){
            revert RebaseToken__InterestRateCanOnlyDecrease(s_interestRate, _newInterestRate);
        }
        s_interestRate = _newInterestRate;
        emit InterestRateSet(_newInterestRate);
    }

    /**
     * @notice Get the principal balance of a user. This is the number of tokens that have currently been minted to the user, not including any interest that has been accrued since the last time that the user interacted with the protocol
     * @param _user The user to get the principal balance for
     * @return The principal balance of the user
     */
    function principalBalanceOf(address _user) external view returns(uint256){
        return super.balanceOf(_user);
    }

    /**
     * @notice Mint the user tokens when they deposit into the vault
     * @param _to The user to mint the tokens to  
     * @param _amount The amount of tokens to mint 
     */
    function mint(address _to, uint256 _amount) external {
        s_userInterestRate[_to] = s_interestRate;
        _mint(_to, _amount);
    }

    /**
     * @notice Burn the user tokens when they withdraw from the vault
     * @param _from The user to burn tokens from
     * @param _amount The amount of tokens to burn
     */
    function burn(address _from, uint256 _amount) external {
        if(_amount == type(uint256).max){
            _amount = balanceOf(_from);
        }
        _mintAccruedInterest(_from);
        _burn(_from, _amount);
    }

    /**
     * @notice Mint the accrued interest to the user since the last time they interacted with the protocol
     * @param _user The user to mint the interest to
     */
    function _mintAccruedInterest(address _user) internal {
        uint256 previousPrincipalBalance = super.balanceOf(_user);
        uint256 currentBalance = balanceOf(_user);
        uint256 balanceIncrease = currentBalance - previousPrincipalBalance;
        // find the current balance of rebase tokens that have been minted to the user -> principal balance
        // calculate their current balance including any interest -> balanceOf
        // mint the difference to the user
        s_userLastUpdatedTimestamp[_user] = block.timestamp;
        _mint(_user, balanceIncrease);
    }

    /**
     * @notice Calculate the balance for the user including the interest rate that has accumulated since the last update
     * (principal balance) + some interest
     * @param _user The user to calculate the balance for
     * @return The final balance of the user
     */
    function balanceOf(address _user) public view override returns(uint256){
        // get the current principal balance
        // multiply the principal balance by the interest that has accumulated since the last update
        return super.balanceOf(_user) * _calculateUserAccumulatedInterestSinceLastUpdate(_user) / PRECISION_FACTOR;
    }

    /**
     * @notice Transfer tokens from one user to another
     * @param _recipient The user to transfer the tokens to
     * @param _amount The amount of tokens to transfer
     * @return True if the transfer was successful
     */
    function transfer(address _recipient, uint256 _amount) public override returns(bool){
        _mintAccruedInterest(_recipient);
        _mintAccruedInterest(msg.sender);
        if(_amount == type(uint256).max){
            _amount = balanceOf(msg.sender);
        }
        if(balanceOf(_recipient) == 0){
            s_userInterestRate[_recipient] = s_userInterestRate[msg.sender];
        }
        return super.transfer(_recipient, _amount);
    }

    /**
     * @notice Transfer tokens from one user to another
     * @param _sender The to transfer the tokens from
     * @param _recipient The user to transfer the tokens to
     * @param _amount The amount of tokens to transfer
     * @return True if the transfer was successful
     */
    function transferFrom(address _sender, address _recipient, uint256 _amount) public override returns(bool){
        _mintAccruedInterest(_recipient);
        _mintAccruedInterest(_sender);
        if(_amount == type(uint256).max){
            _amount = balanceOf(_sender);
        }
        if(balanceOf(_recipient) == 0){
            s_userInterestRate[_recipient] = s_userInterestRate[msg.sender];
        }
        return super.transferFrom(_sender, _recipient, _amount);
    }

    /**
     * @notice Calculate interest that has accumulate since the last update
     * @param _user The user to calculate the interest for
     * @return linearInterest Accumulate interest
     */
    function _calculateUserAccumulatedInterestSinceLastUpdate(address _user) internal view returns(uint256 linearInterest){
        // interest will have linear growth with time
        // principal amount + (principal amount * interest * time elapsed)
        // principal amount (1 + (user interest rate * time elapsed))

        uint256 timeElapsed = block.timestamp - s_userLastUpdatedTimestamp[_user];
        linearInterest = PRECISION_FACTOR + (s_userInterestRate[_user] * timeElapsed);
    }

    /**
     * @notice Get the interest rate that is currently set for the contract. Any future depositors will recieve this rate
     * @return Interest rate for the contract
     */
    function getInterestRate() external view returns(uint256){
        return s_interestRate;
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