/**
 * @author Mikhail Klementev jollheef<AT>riseup.net
 * @license GNU GPLv3
 * @date July 2018
 * @brief appvm launcher
 */

package main

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/digitalocean/go-libvirt"
	"github.com/go-cmd/cmd"
	"github.com/jollheef/go-system"
	"github.com/olekukonko/tablewriter"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

func evalNix(expr string) (s string) {
	command := exec.Command("nix", "eval", "--raw", expr)
	bytes, _ := command.Output()
	s = string(bytes)
	return
}

// Gets an expression returning AppVM config path
func getAppVMExpressionPath(name string) string {
	paths := strings.Split(os.Getenv("APPVM_CONFIGS"), ":")
	for _, a := range paths {
		searchpath := a + "/nix"
		log.Print("Searching " + searchpath + " for expressions")
		if _, err := os.Stat(searchpath); os.IsExist(err) {
			exprpath := searchpath + "/" + name + ".nix"

			if os.Stat(exprpath); os.IsExist(err) {
				return exprpath
			}

		}
		log.Print("Local repo " + searchpath + " doesn't have a nix expression for " + name)
	}
	log.Print("Trying to use remote repo config")

	fetchFormat := "(builtins.fetchurl \"raw.githubusercontent.com/%[1]s/%[2]s/master/nix/%[3]s.nix\" )"
	splitString := strings.Split(name, "/")

	if len(splitString) != 3 {
		// nope, not a repo format
		return evalNix(fmt.Sprintf(fetchFormat, "jollheef", "appvm", name))
	}

	return evalNix(fmt.Sprintf(fetchFormat, splitString[0], splitString[1], splitString[2]))

}

func list(l *libvirt.Libvirt) {
	domains, err := l.Domains()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Started VM:")
	for _, d := range domains {
		if strings.HasPrefix(d.Name, "appvm") {
			fmt.Println("\t", d.Name[6:])
		}
	}

	fmt.Println("\nAvailable VM:")
	files, err := ioutil.ReadDir(os.Getenv("GOPATH") + "/src/code.dumpstack.io/tools/appvm/nix")
	if err != nil {
		log.Fatal(err)
	}

	for _, f := range files {
		fmt.Println("\t", f.Name()[0:len(f.Name())-4])
	}
}

func copyFile(from, to string) (err error) {
	source, err := os.Open(from)
	if err != nil {
		return
	}
	defer source.Close()

	destination, err := os.Create(to)
	if err != nil {
		return
	}

	_, err = io.Copy(destination, source)
	if err != nil {
		destination.Close()
		return
	}

	return destination.Close()
}

func prepareTemplates(appvmPath string) (err error) {
	if _, err = os.Stat(appvmPath + "/nix/local.nix"); os.IsNotExist(err) {
		err = ioutil.WriteFile(configDir+"/nix/local.nix", local_nix_template, 0644)
		if err != nil {
			return
		}
	}

	return
}

func streamStdOutErr(command *cmd.Cmd) {
	for {
		select {
		case line := <-command.Stdout:
			fmt.Println(line)
		case line := <-command.Stderr:
			fmt.Fprintln(os.Stderr, line)
		}
	}
}

func generateVM(name string, verbose bool) (realpath, reginfo, qcow2 string, err error) {
	vmConfigPath := getAppVMExpressionPath(name)
	log.Print(vmConfigPath)
	command := cmd.NewCmdOptions(cmd.Options{Buffered: false, Streaming: true},
		"nix-build", "<nixpkgs/nixos>", "-A", "config.system.build.vm",
		"-I", "nixos-config="+vmConfigPath, "-I", configDir)
	if verbose {
		go streamStdOutErr(command)
	}

	status := <-command.Start()
	if status.Error != nil || status.Exit != 0 {
		log.Println(status.Error, status.Stdout, status.Stderr)
		if status.Error != nil {
			err = status.Error
		} else {
			s := fmt.Sprintf("ret code: %d, out: %v, err: %v",
				status.Exit, status.Stdout, status.Stderr)
			err = errors.New(s)
		}
		return
	}

	realpath, err = filepath.EvalSymlinks("result/system")
	if err != nil {
		return
	}

	bytes, err := ioutil.ReadFile("result/bin/run-nixos-vm")
	if err != nil {
		return
	}

	match := regexp.MustCompile("regInfo=.*/registration").FindSubmatch(bytes)
	if len(match) != 1 {
		err = errors.New("should be one reginfo")
		return
	}

	reginfo = string(match[0])

	syscall.Unlink("result")

	qcow2 = os.Getenv("HOME") + "/appvm/.fake.qcow2"
	if _, err = os.Stat(qcow2); os.IsNotExist(err) {
		system.System("qemu-img", "create", "-f", "qcow2", qcow2, "512M")
		err = os.Chmod(qcow2, 0400) // qemu run with -snapshot, we only need it for create /dev/vda
		if err != nil {
			return
		}
	}

	return
}

func isRunning(l *libvirt.Libvirt, name string) bool {
	_, err := l.DomainLookupByName("appvm_" + name) // yep, there is no libvirt error handling
	// VM is destroyed when stop so NO VM means STOPPED
	return err == nil
}

func generateAppVM(l *libvirt.Libvirt, appvmPath, name string, verbose bool) (err error) {
	err = os.Chdir(appvmPath)
	if err != nil {
		return
	}

	realpath, reginfo, qcow2, err := generateVM(name, verbose)
	if err != nil {
		return
	}

	sharedDir := fmt.Sprintf(os.Getenv("HOME") + "/appvm/" + name)
	os.MkdirAll(sharedDir, 0700)

	xml := generateXML(name, realpath, reginfo, qcow2, sharedDir)
	_, err = l.DomainCreateXML(xml, libvirt.DomainStartValidate)
	return
}

func stupidProgressBar() {
	const length = 70
	for {
		time.Sleep(time.Second / 4)
		fmt.Printf("\r%s]\r[", strings.Repeat(" ", length))
		for i := 0; i <= length-2; i++ {
			time.Sleep(time.Second / 20)
			fmt.Printf("+")
		}
	}
}

func start(l *libvirt.Libvirt, name string, verbose bool) {
	// Currently binary-only installation is not supported, because we need *.nix configurations
	appvmPath := configDir

	// Copy templates
	err := prepareTemplates(appvmPath)
	if err != nil {
		log.Fatal(err)
	}

	if !isRunning(l, name) {
		if !verbose {
			go stupidProgressBar()
		}
		err = generateAppVM(l, appvmPath, name, verbose)
		if err != nil {
			log.Fatal(err)
		}
	}

	cmd := exec.Command("virt-viewer", "-c", "qemu:///system", "appvm_"+name)
	cmd.Start()
}

func stop(l *libvirt.Libvirt, name string) {
	dom, err := l.DomainLookupByName("appvm_" + name)
	if err != nil {
		if libvirt.IsNotFound(err) {
			log.Println("Appvm not found or already stopped")
			return
		} else {
			log.Fatal(err)
		}
	}
	err = l.DomainShutdown(dom)
	if err != nil {
		log.Fatal(err)
	}
}

func drop(name string) {
	appDataPath := fmt.Sprintf(os.Getenv("HOME") + "/appvm/" + name)
	os.RemoveAll(appDataPath)
}

func autoBalloon(l *libvirt.Libvirt, memoryMin, adjustPercent uint64) {
	domains, err := l.Domains()
	if err != nil {
		log.Fatal(err)
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Application VM", "Used memory", "Current memory", "Max memory", "New memory"})
	for _, d := range domains {
		if strings.HasPrefix(d.Name, "appvm_") {
			name := d.Name[6:]

			memoryUsedRaw, err := ioutil.ReadFile(os.Getenv("HOME") + "/appvm/" + name + "/.memory_used")
			if err != nil {
				log.Fatal(err)
			}
			memoryUsedMiB, err := strconv.Atoi(string(memoryUsedRaw[0 : len(memoryUsedRaw)-1]))
			if err != nil {
				log.Fatal(err)
			}
			memoryUsed := memoryUsedMiB * 1024

			_, memoryMax, memoryCurrent, _, _, err := l.DomainGetInfo(d)
			if err != nil {
				log.Fatal(err)
			}

			memoryNew := uint64(float64(memoryUsed) * (1 + float64(adjustPercent)/100))

			if memoryNew > memoryMax {
				memoryNew = memoryMax - 1
			}

			if memoryNew < memoryMin {
				memoryNew = memoryMin
			}

			err = l.DomainSetMemory(d, memoryNew)
			if err != nil {
				log.Fatal(err)
			}

			table.Append([]string{name,
				fmt.Sprintf("%d", memoryUsed),
				fmt.Sprintf("%d", memoryCurrent),
				fmt.Sprintf("%d", memoryMax),
				fmt.Sprintf("%d", memoryNew)})
		}
	}
	table.Render()
}

var configDir = os.Getenv("HOME") + "/.config/appvm/"

func main() {
	os.Mkdir(os.Getenv("HOME")+"/appvm", 0700)

	os.MkdirAll(configDir+"/nix", 0700)

	err := ioutil.WriteFile(configDir+"/nix/base.nix", base_nix, 0644)
	if err != nil {
		log.Fatal(err)
	}

	c, err := net.DialTimeout("unix", "/var/run/libvirt/libvirt-sock", time.Second)
	if err != nil {
		log.Fatal(err)
	}

	l := libvirt.New(c)
	if err := l.Connect(); err != nil {
		log.Fatal(err)
	}
	defer l.Disconnect()

	kingpin.Command("list", "List applications")
	autoballonCommand := kingpin.Command("autoballoon", "Automatically adjust/reduce app vm memory")
	minMemory := autoballonCommand.Flag("min-memory", "Set minimal memory (megabytes)").Default("1024").Uint64()
	adjustPercent := autoballonCommand.Flag("adj-memory", "Adjust memory amount (percents)").Default("20").Uint64()

	startCommand := kingpin.Command("start", "Start application")
	startName := startCommand.Arg("name", "Application name").Required().String()
	startVerbose := startCommand.Flag("verbose", "Increase verbosity").Default("False").Bool()

	stopName := kingpin.Command("stop", "Stop application").Arg("name", "Application name").Required().String()
	dropName := kingpin.Command("drop", "Remove application data").Arg("name", "Application name").Required().String()

	switch kingpin.Parse() {
	case "list":
		list(l)
	case "start":
		start(l, *startName, *startVerbose)
	case "stop":
		stop(l, *stopName)
	case "drop":
		drop(*dropName)
	case "autoballoon":
		autoBalloon(l, *minMemory*1024, *adjustPercent)
	}
}
