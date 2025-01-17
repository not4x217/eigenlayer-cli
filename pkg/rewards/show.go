package rewards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"

	"github.com/Layr-Labs/eigenlayer-cli/pkg/internal/common"
	"github.com/Layr-Labs/eigenlayer-cli/pkg/internal/common/flags"
	"github.com/Layr-Labs/eigenlayer-cli/pkg/internal/erc20"
	"github.com/Layr-Labs/eigenlayer-cli/pkg/telemetry"
	"github.com/Layr-Labs/eigenlayer-cli/pkg/utils"

	"github.com/Layr-Labs/eigenlayer-rewards-proofs/pkg/proofDataFetcher/httpProofDataFetcher"

	"github.com/Layr-Labs/eigensdk-go/chainio/clients/elcontracts"
	"github.com/Layr-Labs/eigensdk-go/logging"
	eigenSdkUtils "github.com/Layr-Labs/eigensdk-go/utils"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urfave/cli/v2"
)

type ClaimType string

type ELReader interface {
	GetCumulativeClaimed(ctx context.Context, earnerAddress, tokenAddress gethcommon.Address) (*big.Int, error)
}

const (
	All       ClaimType = "all"
	Unclaimed ClaimType = "unclaimed"
	Claimed   ClaimType = "claimed"

	LatestTimestamp       = "latest"
	LatestActiveTimestamp = "latest_active"
)

func ShowCmd(p utils.Prompter) *cli.Command {
	showCmd := &cli.Command{
		Name:      "show",
		Usage:     "Show rewards for an address against the `DistributionRoot` posted on-chain by the rewards updater",
		UsageText: "show",
		Description: `
Command to show rewards for earners

Helpful flags
- claim-type: Type of rewards to show. Can be 'all', 'claimed' or 'unclaimed'
- claim-timestamp: Timestamp of the claim distribution root to use. Can be 'latest' or 'latest_active'.
	- 'latest' will show rewards for the latest root (can contain non-claimable rewards)
	- 'latest_active' will show rewards for the latest active root (only claimable rewards)
		`,
		After: telemetry.AfterRunAction(),
		Flags: getShowFlags(),
		Action: func(cCtx *cli.Context) error {
			return ShowRewards(cCtx)
		},
	}

	return showCmd
}

func getShowFlags() []cli.Flag {
	baseFlags := []cli.Flag{
		&flags.NetworkFlag,
		&flags.OutputFileFlag,
		&flags.OutputTypeFlag,
		&flags.VerboseFlag,
		&flags.ETHRpcUrlFlag,
		&EarnerAddressFlag,
		&EnvironmentFlag,
		&ClaimTypeFlag,
		&ProofStoreBaseURLFlag,
		&ClaimTimestampFlag,
	}

	sort.Sort(cli.FlagsByName(baseFlags))
	return baseFlags
}

func ShowRewards(cCtx *cli.Context) error {
	ctx := cCtx.Context
	logger := common.GetLogger(cCtx)

	config, err := readAndValidateConfig(cCtx, logger)
	if err != nil {
		return fmt.Errorf("error reading and validating config: %s", err)
	}
	cCtx.App.Metadata["network"] = config.ChainID.String()

	ethClient, err := ethclient.Dial(config.RPCUrl)
	if err != nil {
		return eigenSdkUtils.WrapError("failed to create new eth client", err)
	}

	elReader, err := elcontracts.NewReaderFromConfig(
		elcontracts.Config{
			RewardsCoordinatorAddress: config.RewardsCoordinatorAddress,
		},
		ethClient,
		logger,
	)
	if err != nil {
		return eigenSdkUtils.WrapError("failed to create new reader from config", err)
	}

	df := httpProofDataFetcher.NewHttpProofDataFetcher(
		config.ProofStoreBaseURL,
		config.Environment,
		config.Network,
		http.DefaultClient,
	)

	claimDate, _, err := getClaimDistributionRoot(ctx, config.ClaimTimestamp, elReader, logger)
	if err != nil {
		return eigenSdkUtils.WrapError("failed to get claim distribution root", err)
	}

	proofData, err := df.FetchClaimAmountsForDate(ctx, claimDate)
	if err != nil {
		return eigenSdkUtils.WrapError("failed to fetch claim amounts for date", err)
	}

	tokenAddressesMap, present := proofData.Distribution.GetTokensForEarner(config.EarnerAddress)
	if !present {
		return eigenSdkUtils.WrapError("earner address not found in distribution", nil)
	}

	allRewards := make(map[gethcommon.Address]*big.Int)
	msg := "Lifetime Rewards"
	for pair := tokenAddressesMap.Oldest(); pair != nil; pair = pair.Next() {
		amt, _ := new(big.Int).SetString(pair.Value.String(), 10)
		allRewards[pair.Key] = amt
	}

	if config.ClaimType != All {
		claimedRewards, err := getClaimedRewards(ctx, elReader, config.EarnerAddress, allRewards)
		if err != nil {
			return eigenSdkUtils.WrapError("failed to get claimed rewards", err)
		}
		switch config.ClaimType {
		case Claimed:
			allRewards = claimedRewards
			msg = "Claimed Rewards"
		case Unclaimed:
			allRewards = calculateUnclaimedRewards(allRewards, claimedRewards)
			msg = "Unclaimed Rewards"
		}
	}
	err = handleRewardsOutput(config, allRewards, msg)
	if err != nil {
		return err
	}
	return nil
}

func getClaimedRewards(
	ctx context.Context,
	elReader ELReader,
	earnerAddress gethcommon.Address,
	allRewards map[gethcommon.Address]*big.Int,
) (map[gethcommon.Address]*big.Int, error) {
	claimedRewards := make(map[gethcommon.Address]*big.Int)
	for address := range allRewards {
		claimed, err := getCummulativeClaimedRewards(ctx, elReader, earnerAddress, address)
		if err != nil {
			return nil, err
		}
		claimedRewards[address] = claimed
	}
	return claimedRewards, nil
}

func getCummulativeClaimedRewards(
	ctx context.Context,
	elReader ELReader,
	earnerAddress gethcommon.Address,
	tokenAddress gethcommon.Address,
) (*big.Int, error) {
	claimed, err := elReader.GetCumulativeClaimed(ctx, earnerAddress, tokenAddress)
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		claimed = big.NewInt(0)
	}
	return claimed, nil
}

func calculateUnclaimedRewards(
	allRewards,
	claimedRewards map[gethcommon.Address]*big.Int,
) map[gethcommon.Address]*big.Int {
	unclaimedRewards := make(map[gethcommon.Address]*big.Int)
	for address, total := range allRewards {
		claimed := claimedRewards[address]
		unclaimedRewards[address] = new(big.Int).Sub(total, claimed)
	}
	return unclaimedRewards
}

func handleRewardsOutput(
	cfg *ShowConfig,
	rewards map[gethcommon.Address]*big.Int,
	msg string,
) error {
	client, err := ethclient.Dial(cfg.RPCUrl)
	if err != nil {
		return err
	}
	allRewards := make(allRewardsJson, 0)
	for address, amount := range rewards {
		allRewards = append(allRewards, rewardsJson{
			TokenName: erc20.GetTokenName(address, client),
			Address:   address.Hex(),
			Amount:    amount.String(),
		})
	}
	if cfg.OutputType == "json" {
		out, err := json.MarshalIndent(allRewards, "", "  ")
		if err != nil {
			return err
		}
		if cfg.Output != "" {
			return common.WriteToFile(out, cfg.Output)
		} else {
			fmt.Println(string(out))
		}
	} else {
		fmt.Println()
		if cfg.ClaimTimestamp == LatestTimestamp {
			fmt.Println("> Showing rewards for latest root (can contain non-claimable rewards)")
		} else {
			fmt.Println("> Showing rewards for latest active root (only claimable rewards)")
		}
		fmt.Println()
		fmt.Println(strings.Repeat("-", 30), msg, strings.Repeat("-", 30))
		printRewards(allRewards)
	}
	return nil
}

func printRewards(allRewards allRewardsJson) {
	// Define column headers and widths
	headers := []string{
		"Token Name",
		"Token Address",
		"Amount (Wei)",
	}
	widths := []int{20, 46, 30}

	// print dashes
	for _, width := range widths {
		fmt.Print("+" + strings.Repeat("-", width+1))
	}
	fmt.Println("+")

	// Print header
	for i, header := range headers {
		fmt.Printf("| %-*s", widths[i], header)
	}
	fmt.Println("|")

	// Print separator
	for _, width := range widths {
		fmt.Print("|", strings.Repeat("-", width+1))
	}
	fmt.Println("|")

	// Print data rows
	for _, rewards := range allRewards {
		fmt.Printf("| %-*s| %-*s| %-*s|\n",
			widths[0], rewards.TokenName,
			widths[1], rewards.Address,
			widths[2], rewards.Amount,
		)
	}

	// print dashes
	for _, width := range widths {
		fmt.Print("+" + strings.Repeat("-", width+1))
	}
	fmt.Println("+")
}

func readAndValidateConfig(cCtx *cli.Context, logger logging.Logger) (*ShowConfig, error) {
	earnerAddress := gethcommon.HexToAddress(cCtx.String(EarnerAddressFlag.Name))
	output := cCtx.String(flags.OutputFileFlag.Name)
	outputType := cCtx.String(flags.OutputTypeFlag.Name)
	ethRpcUrl := cCtx.String(flags.ETHRpcUrlFlag.Name)
	network := cCtx.String(flags.NetworkFlag.Name)
	env := cCtx.String(EnvironmentFlag.Name)
	if env == "" {
		env = getEnvFromNetwork(network)
	}
	logger.Debugf("Network: %s, Env: %s", network, env)
	rewardsCoordinatorAddress := cCtx.String(RewardsCoordinatorAddressFlag.Name)

	var err error
	if common.IsEmptyString(rewardsCoordinatorAddress) {
		rewardsCoordinatorAddress, err = common.GetRewardCoordinatorAddress(utils.NetworkNameToChainId(network))
		if err != nil {
			return nil, err
		}
	}
	logger.Debugf("Using Rewards Coordinator address: %s", rewardsCoordinatorAddress)

	proofStoreBaseURL := cCtx.String(ProofStoreBaseURLFlag.Name)

	// If empty get from utils
	if common.IsEmptyString(proofStoreBaseURL) {
		proofStoreBaseURL = getProofStoreBaseURL(network)

		// If still empty return error
		if common.IsEmptyString(proofStoreBaseURL) {
			return nil, errors.New("proof store base URL not provided")
		}
	}
	logger.Debugf("Using Proof store base URL: %s", proofStoreBaseURL)

	claimType := ClaimType(cCtx.String(ClaimTypeFlag.Name))
	if claimType != All && claimType != Unclaimed && claimType != Claimed {
		return nil, errors.New("claim type must be 'all', 'unclaimed' or 'claimed'")
	}
	logger.Debugf("Claim Type: %s", claimType)

	claimTimestamp := cCtx.String(ClaimTimestampFlag.Name)
	if claimTimestamp != LatestTimestamp && claimTimestamp != LatestActiveTimestamp {
		return nil, errors.New("claim timestamp must be 'latest' or 'latest_active'")
	}

	chainID := utils.NetworkNameToChainId(network)
	logger.Debugf("Using chain ID: %s", chainID.String())

	// TODO(shrimalmadhur): Fix to make sure correct S3 bucket is used. Clean up later
	if network == utils.MainnetNetworkName {
		network = "ethereum"
	}

	return &ShowConfig{
		EarnerAddress:             earnerAddress,
		Network:                   network,
		Environment:               env,
		ClaimType:                 claimType,
		ChainID:                   chainID,
		Output:                    output,
		OutputType:                outputType,
		RPCUrl:                    ethRpcUrl,
		ProofStoreBaseURL:         proofStoreBaseURL,
		ClaimTimestamp:            claimTimestamp,
		RewardsCoordinatorAddress: gethcommon.HexToAddress(rewardsCoordinatorAddress),
	}, nil
}
