package utils

import (
	"os"
	"strconv"

	log "github.com/sirupsen/logrus"
)

func GetEnvInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		} else {
			log.Errorf("Invalid integer value for %s: %s. Using fallback value: %d.\n", key, value, fallback)
			return fallback
		}
	}
	return fallback
}
