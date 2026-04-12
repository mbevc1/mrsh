package main

import "github.com/mbevc1/mrsh/cmd"

func main() {
	cmd.Name = Name
	cmd.Execute(version, commit, date, builtBy)
}
