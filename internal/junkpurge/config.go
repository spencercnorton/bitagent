package junkpurge

type Config struct {
	QuarantineDays     int
	MaintenanceEnabled bool
}

func NewDefaultConfig() Config { return Config{QuarantineDays: 30} }
