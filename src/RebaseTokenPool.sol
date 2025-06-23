// SPDX-License-Identifier: MIT

pragma solidity ^0.8.24;

import {TokenPool} from "@ccip/contracts/src/v0.8/ccip/pools/TokenPool.sol";
import {IERC20} from "@ccip/contracts/src/v0.8/vendor/openzeppelin-solidity/v4.8.3/contracts/token/ERC20/IERC20.sol";
import {Pool} from "@ccip/contracts/src/v0.8/ccip/libraries/Pool.sol";
import {IRebaseToken} from "./interfaces/IRebaseToken.sol";

// responsible for burning tokens on source chain and minting the same amount on the destination chain
// in this protocol we are only allowing bridging same types of token
contract RebaseTokenPool is TokenPool {
    constructor(IERC20 _token, address[] memory _allowlist, address _rmnProxy, address _router)
        TokenPool(_token, 18, _allowlist, _rmnProxy, _router)
    {}

    function lockOrBurn(Pool.LockOrBurnInV1 calldata lockOrBurnIn)
        external
        returns (Pool.LockOrBurnOutV1 memory lockOrBurnOut)
    {
        _validateLockOrBurn(lockOrBurnIn);
        // we want to get the interest rate of the user who originally sent the transaction
        uint256 userInterestRate = IRebaseToken(address(i_token)).getUserInterestRate(lockOrBurnIn.originalSender);
        // the tokens to be burnt are held by this contract, hence we use address(this)
        IRebaseToken(address(i_token)).burn(address(this), lockOrBurnIn.amount);

        lockOrBurnOut = Pool.LockOrBurnOutV1({
            destTokenAddress: getRemoteToken(lockOrBurnIn.remoteChainSelector),
            destPoolData: abi.encode(userInterestRate)
        });
    }

    function releaseOrMint(Pool.ReleaseOrMintInV1 calldata releaseOrMintIn)
        external
        returns (Pool.ReleaseOrMintOutV1 memory releaseOrMintOut)
    {
        _validateReleaseOrMint(releaseOrMintIn);
        uint256 userInterestRate = abi.decode(releaseOrMintIn.sourcePoolData, (uint256));
        address receiver = releaseOrMintIn.receiver;

        IRebaseToken(address(i_token)).mint(receiver, releaseOrMintIn.amount, userInterestRate);
        return Pool.ReleaseOrMintOutV1({destinationAmount: releaseOrMintIn.amount});
    }
}
