// SPDX-License-Identifier: MIT

pragma solidity ^0.8.24;

import {Test, console} from "forge-std/Test.sol";
import {RebaseToken} from "../src/RebaseToken.sol";
import {RebaseTokenPool} from "../src/RebaseTokenPool.sol";
import {Vault} from "../src/Vault.sol";
import {IRebaseToken} from "../src/interfaces/IRebaseToken.sol";
import {CCIPLocalSimulatorFork, Register} from "@chainlink-local/src/ccip/CCIPLocalSimulatorFork.sol";
import {IERC20} from "@ccip/contracts/src/v0.8/vendor/openzeppelin-solidity/v4.8.3/contracts/token/ERC20/IERC20.sol";
import {RegistryModuleOwnerCustom} from "@ccip/contracts/src/v0.8/ccip/tokenAdminRegistry/RegistryModuleOwnerCustom.sol";
import {TokenAdminRegistry} from "@ccip/contracts/src/v0.8/ccip/tokenAdminRegistry/TokenAdminRegistry.sol";
import {TokenPool} from "@ccip/contracts/src/v0.8/ccip/pools/TokenPool.sol";
import {RateLimiter} from "@ccip/contracts/src/v0.8/ccip/libraries/RateLimiter.sol";
import {Client} from "@ccip/contracts/src/v0.8/ccip/libraries/Client.sol";
import {IRouterClient} from "@ccip/contracts/src/v0.8/ccip/interfaces/IRouterClient.sol";

contract CrossChainTest is Test{
    address owner = makeAddr("owner");
    address alice = makeAddr("alice");
    uint256 public SEND_VALUE = 1e5;
    // two forks simulating two different chains
    uint256 sepoliaFork;
    uint256 arbSepoliaFork;
    CCIPLocalSimulatorFork ccipLocalSimulatorFork;

    RebaseToken destRebaseToken;
    RebaseToken sourceRebaseToken;

    Vault vault;

    RebaseTokenPool destPool;
    RebaseTokenPool sourcePool;

    TokenAdminRegistry tokenAdminRegistrySepolia;
    TokenAdminRegistry tokenAdminRegistryArbSepolia;

    Register.NetworkDetails sepoliaNetworkDetails;
    Register.NetworkDetails arbSepoliaNetworkDetails;

    RegistryModuleOwnerCustom registryModuleOwnerCustomSepolia;
    RegistryModuleOwnerCustom registryModuleOwnerCustomArbSepolia;

    function setUp() public {
        address[] memory allowlist = new address[](0);

        sepoliaFork = vm.createSelectFork("eth");
        arbSepoliaFork = vm.createFork("arb");

        ccipLocalSimulatorFork = new CCIPLocalSimulatorFork();
        vm.makePersistent(address(ccipLocalSimulatorFork));

        sepoliaNetworkDetails = ccipLocalSimulatorFork.getNetworkDetails(block.chainid);
        vm.startPrank(owner);
        sourceRebaseToken = new RebaseToken();
        // deploying token pool on sepolia
        sourcePool = new RebaseTokenPool(
            IERC20(address(sourceRebaseToken)),
            allowlist,
            sepoliaNetworkDetails.rmnProxyAddress,
            sepoliaNetworkDetails.routerAddress
        );
        // deploying the vault
        vault = new Vault(IRebaseToken(address(sourceRebaseToken)));
        // giving some liquidity funds to the wallet
        vm.deal(address(vault), 1e18);

        sourceRebaseToken.grantMintAndBurnRole(address(vault));
        sourceRebaseToken.grantMintAndBurnRole(address(sourcePool));

        // claiming the admin role on sepolia
        registryModuleOwnerCustomSepolia = RegistryModuleOwnerCustom(sepoliaNetworkDetails.registryModuleOwnerCustomAddress);
        registryModuleOwnerCustomSepolia.registerAdminViaOwner(address(sourceRebaseToken));
        // accepting the role on sepolia 
        tokenAdminRegistrySepolia = TokenAdminRegistry(sepoliaNetworkDetails.tokenAdminRegistryAddress);
        tokenAdminRegistrySepolia.acceptAdminRole(address(sourceRebaseToken));
        // linking token to pool in the token admin registry on sepolia
        tokenAdminRegistrySepolia.setPool(address(sourceRebaseToken), address(sourcePool));
        // now the pool for the specefic token is set, and we dont need to manually call the mint function while bridging
        vm.stopPrank();

        // Deploying and configuring the destination chain 
        vm.selectFork(arbSepoliaFork);
        vm.startPrank(owner);
        // deploying the token on arbitrum
        arbSepoliaNetworkDetails = ccipLocalSimulatorFork.getNetworkDetails(block.chainid);
        destRebaseToken = new RebaseToken();
        // deploying the token pool
        destPool = new RebaseTokenPool(
            IERC20(address(destRebaseToken)),
            allowlist,
            arbSepoliaNetworkDetails.rmnProxyAddress,
            arbSepoliaNetworkDetails.routerAddress
        );
        destRebaseToken.grantMintAndBurnRole(address(destPool));
        // claiming the role on arbitrum
        registryModuleOwnerCustomArbSepolia = RegistryModuleOwnerCustom(arbSepoliaNetworkDetails.registryModuleOwnerCustomAddress);
        registryModuleOwnerCustomArbSepolia.registerAdminViaOwner(address(destRebaseToken));
        // accepting the admin role 
        tokenAdminRegistryArbSepolia = TokenAdminRegistry(arbSepoliaNetworkDetails.tokenAdminRegistryAddress);
        tokenAdminRegistryArbSepolia.acceptAdminRole(address(destRebaseToken));
        tokenAdminRegistryArbSepolia.setPool(address(destRebaseToken), address(destPool));
        vm.stopPrank();
    }

    function configureTokenPool(
        uint256 fork,
        TokenPool localPool,
        TokenPool remotePool,
        IRebaseToken remoteToken,
        Register.NetworkDetails memory remoteNetworkDetails
    ) public {
        // configures the local token pool contract to recognize the remote pool on remote chain
        vm.selectFork(fork);
        vm.startPrank(owner);
        TokenPool.ChainUpdate[] memory chains = new TokenPool.ChainUpdate[](1);
        bytes[] memory remotePoolAddresses = new bytes[](1);
        remotePoolAddresses[0] = abi.encode(address(remotePool));
        chains[0] = TokenPool.ChainUpdate({
            remoteChainSelector: remoteNetworkDetails.chainSelector,
            remotePoolAddresses: remotePoolAddresses,
            remoteTokenAddress: abi.encode(address(remoteToken)),
            outboundRateLimiterConfig: RateLimiter.Config({isEnabled: false, capacity: 0, rate: 0}),
            inboundRateLimiterConfig: RateLimiter.Config({isEnabled: false, capacity: 0, rate: 0})
        });
        uint64[] memory remoteChainSelectorsToRemove = new uint64[](0);
        localPool.applyChainUpdates(remoteChainSelectorsToRemove, chains);
        vm.stopPrank();
    }

    function bridgeTokens(
        uint256 amountToBridge,
        uint256 localFork,
        uint256 remoteFork,
        Register.NetworkDetails memory localNetworkDetails,
        Register.NetworkDetails memory remoteNetworkDetails,
        RebaseToken localToken,
        RebaseToken remoteToken
    ) public {
        vm.selectFork(localFork);
        vm.startPrank(alice);
        Client.EVMTokenAmount[] memory tokenToSendDetails = new Client.EVMTokenAmount[](1);
        Client.EVMTokenAmount memory tokenAmount = Client.EVMTokenAmount({
            token: address(localToken),
            amount: amountToBridge
        });
        tokenToSendDetails[0] = tokenAmount;
        // appriving the router address to spend tokens
        IERC20(address(localToken)).approve(localNetworkDetails.routerAddress, amountToBridge);

        Client.EVM2AnyMessage memory message = Client.EVM2AnyMessage({
            receiver: abi.encode(alice),
            data: "",
            tokenAmounts: tokenToSendDetails,
            extraArgs: "",
            feeToken: localNetworkDetails.linkAddress
        });
        vm.stopPrank();

        // getting link from faucet so alice can pay gas fees in link
        // message has to be passed because the fee depends on the message content
        ccipLocalSimulatorFork.requestLinkFromFaucet(
            alice, IRouterClient(localNetworkDetails.routerAddress).getFee(remoteNetworkDetails.chainSelector, message)
        );

        vm.startPrank(alice);
        IERC20(localNetworkDetails.linkAddress).approve(
            localNetworkDetails.routerAddress,
            IRouterClient(localNetworkDetails.routerAddress).getFee(remoteNetworkDetails.chainSelector, message)
        );
        uint256 balanceBeforeBridge = IERC20(address(localToken)).balanceOf(alice);
        console.log("Local balance before bridging: %d", balanceBeforeBridge);
        IRouterClient(localNetworkDetails.routerAddress).ccipSend(remoteNetworkDetails.chainSelector, message);
        uint256 sourceBalanceAfterBridge = IERC20(address(localToken)).balanceOf(alice);
        console.log("Local balance after bridging: %d", sourceBalanceAfterBridge);
        assertEq(sourceBalanceAfterBridge, balanceBeforeBridge - amountToBridge);
        vm.stopPrank();

        vm.selectFork(remoteFork);
        vm.warp(block.timestamp + 900);

        uint256 initalArbBalance = IERC20(address(remoteToken)).balanceOf(alice);
        console.log("Remote balance before bridging: %d", initalArbBalance);
        // on real chains this happens automatically
        // while testing we have to trigger this step manually
        // retrives pending message with last ccipSend from the given fork

        ccipLocalSimulatorFork.switchChainAndRouteMessage(remoteFork);

        // message was routed and the mint function on the pool contract was called
        console.log("Remote user interest rate: %d", remoteToken.getUserInterestRate(alice));
        uint256 destBalance = IERC20(address(remoteToken)).balanceOf(alice);
        console.log("Remote balance after bridging: %d", destBalance);
        assertEq(destBalance, initalArbBalance + amountToBridge);
    }

    function testBridgeAllTokens() public {
        configureTokenPool(
            sepoliaFork, sourcePool, destPool, IRebaseToken(address(destRebaseToken)), arbSepoliaNetworkDetails
        );
        configureTokenPool(
            arbSepoliaFork, destPool, sourcePool, IRebaseToken(address(sourceRebaseToken)), sepoliaNetworkDetails
        );
        vm.selectFork(sepoliaFork);
        vm.deal(alice, SEND_VALUE);
        vm.startPrank(alice);
        Vault(payable(address(vault))).deposit{value: SEND_VALUE}();
        console.log("Bridging %d tokens", SEND_VALUE);
        assertEq(IERC20(address(sourceRebaseToken)).balanceOf(alice), SEND_VALUE);
        assertEq(IERC20(address(destRebaseToken)).balanceOf(alice), 0);
        vm.stopPrank();

        bridgeTokens(
            SEND_VALUE, 
            sepoliaFork, 
            arbSepoliaFork, 
            sepoliaNetworkDetails, 
            arbSepoliaNetworkDetails,
            sourceRebaseToken, 
            destRebaseToken
        );
        assertEq(IERC20(address(sourceRebaseToken)).balanceOf(alice), 0);
        assertEq(IERC20(address(destRebaseToken)).balanceOf(alice), SEND_VALUE);
    }

    function testBridgeAllTokensBack() public {
        configureTokenPool(
            sepoliaFork, sourcePool, destPool, IRebaseToken(address(destRebaseToken)), arbSepoliaNetworkDetails
        );
        configureTokenPool(
            arbSepoliaFork, destPool, sourcePool, IRebaseToken(address(sourceRebaseToken)), sepoliaNetworkDetails
        );
        vm.selectFork(sepoliaFork);
        vm.deal(alice, SEND_VALUE);
        vm.startPrank(alice);
        Vault(payable(address(vault))).deposit{value: SEND_VALUE}();
        console.log("Bridging %d tokens", SEND_VALUE);
        assertEq(IERC20(address(sourceRebaseToken)).balanceOf(alice), SEND_VALUE);
        assertEq(IERC20(address(sourceRebaseToken)).balanceOf(alice), SEND_VALUE);
        vm.stopPrank();

        bridgeTokens(
            SEND_VALUE,
            sepoliaFork,
            arbSepoliaFork,
            sepoliaNetworkDetails,
            arbSepoliaNetworkDetails,
            sourceRebaseToken, 
            destRebaseToken
        );

        // bridging the tokens back 
        vm.selectFork(arbSepoliaFork);
        console.log("User balance before warp: %d", destRebaseToken.balanceOf(alice));
        vm.warp(block.timestamp + 3600);
        console.log("User balance after warp: %d", destRebaseToken.balanceOf(alice));
        uint256 destBalance = IERC20(address(destRebaseToken)).balanceOf(alice);
        console.log("Amount bridging back %d tokens", destBalance);
        
        bridgeTokens(
            destBalance,
            arbSepoliaFork,
            sepoliaFork,
            arbSepoliaNetworkDetails,
            sepoliaNetworkDetails,
            destRebaseToken, 
            sourceRebaseToken
        );
    }
}