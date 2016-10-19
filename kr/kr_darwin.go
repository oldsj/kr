package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/urfave/cli"
)

var plist = os.Getenv("HOME") + "/Library/LaunchAgents/co.krypt.krd.plist"

func restartCommand(c *cli.Context) (err error) {
	exec.Command("launchctl", "unload", plist).Run()
	err = exec.Command("launchctl", "load", plist).Run()
	if err != nil {
		PrintFatal("Failed to restart Kryptonite daemon.")
	}
	fmt.Println("Restarted Kryptonite daemon.")
	return
}

func openBrowser(url string) {
	exec.Command("open", url).Run()
}

func uninstallCommand(c *cli.Context) (err error) {
	confirmOrFatal("Uninstall Kryptonite from this workstation?")
	exec.Command("brew", "uninstall", "kr").Run()
	os.Remove("/usr/local/bin/kr")
	os.Remove("/usr/local/bin/krd")
	os.Remove("/usr/local/lib/kr-pkcs11.so")
	exec.Command("launchctl", "unload", plist).Run()
	os.Remove(plist)
	exec.Command(cleanSSHConfigCommand[0], cleanSSHConfigCommand[1:]...).Run()
	PrintErr("Kryptonite uninstalled.")
	return
}
