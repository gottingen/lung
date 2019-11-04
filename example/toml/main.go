package main

import (
	"fmt"
	"github.com/gottingen/lung"
)


func main() {

	lung.SetConfigName("conf")     // no need to include file extension
	lung.AddConfigPath("config")  // set the path of your config file

	err := lung.ReadInConfig()
	if err != nil {
		fmt.Println("Config file not found...")
	} else {
		dev_server := lung.GetString("development.server")
		dev_connection_max := lung.GetInt("development.connection_max")
		dev_enabled := lung.GetBool("development.enabled")
		dev_port := lung.GetInt("development.port")

		fmt.Printf("\nDevelopment Config found:\n server = %s\n connection_max = %d\n" +
			" enabled = %t\n" +
			" port = %d\n",
			dev_server,
			dev_connection_max,
			dev_enabled,
			dev_port)

		prod_server := lung.GetString("production.server")
		prod_connection_max := lung.GetInt("production.connection_max")
		prod_enabled := lung.GetBool("production.enabled")
		prod_port := lung.GetInt("production.port")

		fmt.Printf("\nProduction Config found:\n server = %s\n connection_max = %d\n" +
			" enabled = %t\n" +
			" port = %d\n",
			prod_server,
			prod_connection_max,
			prod_enabled,
			prod_port)
	}

}
