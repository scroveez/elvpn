/*
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 * Author: Mahmoud Abdelsalam <scroveez@gmail.com>
 *
 */

package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
        
        "github.com/scroveez/elvpn/el"
	. "github.com/scroveez/elvpn/internal"
)

var srvMode, cltMode, debug, getVersion bool
var cfgFile string

var VERSION = "0.0.1"

func main() {
	flag.BoolVar(&getVersion, "version", false, "Get Version info")
	flag.BoolVar(&debug, "debug", false, "Provide debug info")
	flag.StringVar(&cfgFile, "config", "", "configfile")
	flag.Parse()

	if getVersion {
		fmt.Println("ElVPN: VPN for engineers!")
		fmt.Printf("Version: %s\n", VERSION)
		os.Exit(0)
	}


	InitLogger(debug)
	logger := GetLogger()

	checkerr := func(err error) {
		if err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}
	}

	if cfgFile == "" {
		cfgFile = flag.Arg(0)
	}

	logger.Info("using config file: ", cfgFile)

	icfg, err := el.ParseElConfig(cfgFile)
	logger.Debug("%v", icfg)
	checkerr(err)

	maxProcs := runtime.GOMAXPROCS(0)
	if maxProcs < 2 {
		runtime.GOMAXPROCS(2)
	}

	switch cfg := icfg.(type) {
	case el.ElServerConfig:
		err := el.NewServer(cfg)
		checkerr(err)
                fmt.Printf("Server\n")
	case el.ElClientConfig:
		err := el.NewClient(cfg)
		checkerr(err)
                fmt.Printf("Client \n")
	default:
		logger.Error("Invalid config file")
	}

	
}
