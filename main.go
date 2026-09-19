package main

import (
	_ "github.com/joho/godotenv/autoload"
	"github.com/spencercnorton/bitagent/internal/app"
)

func main() {
	app.New().Run()
}
