package invite

import (
	"errors"
	"math/big"
)

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var (
	base58Radix = big.NewInt(58)
	base58Index = func() [256]int16 {
		var index [256]int16
		for position := range index {
			index[position] = -1
		}
		for position, character := range []byte(base58Alphabet) {
			index[character] = int16(position)
		}
		return index
	}()
)

func encodeBase58(input []byte) string {
	if len(input) == 0 {
		return ""
	}
	zeroes := 0
	for zeroes < len(input) && input[zeroes] == 0 {
		zeroes++
	}
	value := new(big.Int).SetBytes(input)
	encoded := make([]byte, 0, len(input)*138/100+1)
	remainder := new(big.Int)
	for value.Sign() > 0 {
		value.QuoRem(value, base58Radix, remainder)
		encoded = append(encoded, base58Alphabet[remainder.Int64()])
	}
	for range zeroes {
		encoded = append(encoded, base58Alphabet[0])
	}
	for left, right := 0, len(encoded)-1; left < right; left, right = left+1, right-1 {
		encoded[left], encoded[right] = encoded[right], encoded[left]
	}
	return string(encoded)
}

func decodeBase58(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, errors.New("base58 value is empty")
	}
	zeroes := 0
	for zeroes < len(encoded) && encoded[zeroes] == base58Alphabet[0] {
		zeroes++
	}
	value := new(big.Int)
	digit := new(big.Int)
	for index := 0; index < len(encoded); index++ {
		character := encoded[index]
		decoded := base58Index[character]
		if decoded < 0 {
			return nil, errors.New("base58 character is invalid")
		}
		value.Mul(value, base58Radix)
		digit.SetInt64(int64(decoded))
		value.Add(value, digit)
	}
	decoded := value.Bytes()
	result := make([]byte, zeroes+len(decoded))
	copy(result[zeroes:], decoded)
	return result, nil
}
