package authentication

import "time"

type Observer interface {
	RedisOperation(operation string, duration time.Duration, err error)
	RedisEvent(event, result string)
}
