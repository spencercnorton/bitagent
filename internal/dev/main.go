package main

import (
	_ "github.com/joho/godotenv/autoload"
	"github.com/spencercnorton/bitagent/internal/dev/app"
)

func main() {
	app.New().Run()
}
