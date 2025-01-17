package utils

import "github.com/ethereum/go-ethereum/common"

const (
	EmojiCheckMark = "✅"
	EmojiCrossMark = "❌"
	EmojiWarning   = "⚠️"
	EmojiInfo      = "ℹ️"
	EmojiWait      = "⏳"
	EmojiLink      = "🔗"
	EmojiInternet  = "🌐"

	MainnetChainId           = 1
	HoleskyChainId           = 17000
	AnvilChainId             = 31337
	BuilderPlaygroundChainId = 1337

	MainnetNetworkName    = "mainnet"
	HoleskyNetworkName    = "holesky"
	AnvilNetworkName      = "anvil"
	BuilderPlaygroundName = "builder-playground"
	UnknownNetworkName    = "unknown"
)

var (
	ZeroAddress = common.HexToAddress("")
)
