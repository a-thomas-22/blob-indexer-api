package api

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	ethDecimals  = 18
	gweiDecimals = 9
)

var explorerBaseURLs = map[int]string{
	1:        "https://etherscan.io",
	11155111: "https://sepolia.etherscan.io",
}

type explorerURLs struct {
	Transaction string
	Address     string
	Block       string
}

func explorerURLsForBlob(chainID int, txHash, address string, blockNumber int64, confirmed bool) explorerURLs {
	baseURL := explorerBaseURLs[chainID]
	if baseURL == "" {
		return explorerURLs{}
	}

	urls := explorerURLs{}
	if common.IsHexHash(txHash) {
		urls.Transaction = fmt.Sprintf("%s/tx/%s", baseURL, txHash)
	}
	if common.IsHexAddress(address) {
		urls.Address = fmt.Sprintf("%s/address/%s", baseURL, address)
	}
	if confirmed && blockNumber > 0 {
		urls.Block = fmt.Sprintf("%s/block/%d", baseURL, blockNumber)
	}

	return urls
}

func formatWeiAsETH(wei string) string {
	return formatIntegerDecimal(wei, ethDecimals)
}

func formatWeiAsGwei(wei string) string {
	return formatIntegerDecimal(wei, gweiDecimals)
}

func formatOptionalWeiAsGwei(wei *string) string {
	if wei == nil {
		return ""
	}
	return formatWeiAsGwei(*wei)
}

func formatIntegerDecimal(raw string, decimals int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || decimals < 0 {
		return ""
	}

	value, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return ""
	}

	if value.Sign() == 0 || decimals == 0 {
		return value.String()
	}

	sign := ""
	if value.Sign() < 0 {
		sign = "-"
		value.Abs(value)
	}

	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	integer := new(big.Int)
	fraction := new(big.Int)
	integer.QuoRem(value, divisor, fraction)
	if fraction.Sign() == 0 {
		return sign + integer.String()
	}

	fractionText := fraction.String()
	if len(fractionText) < decimals {
		fractionText = strings.Repeat("0", decimals-len(fractionText)) + fractionText
	}
	fractionText = strings.TrimRight(fractionText, "0")

	return sign + integer.String() + "." + fractionText
}
