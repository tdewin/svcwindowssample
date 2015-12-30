package main

import (
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/debug"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
	"log"
	"os"
	"fmt"
	"strings"
	"path/filepath"
	"time"
	"net/http"
	"sync"
	"github.com/braintree/manners"
)


func exePath() (string, error) {
	prog := os.Args[0]
	p, err := filepath.Abs(prog)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	if err == nil {
		if !fi.Mode().IsDir() {
			return p, nil
		}
		err = fmt.Errorf("%s is directory", p)
	}
	if filepath.Ext(p) == "" {
		p += ".exe"
		fi, err := os.Stat(p)
		if err == nil {
			if !fi.Mode().IsDir() {
				return p, nil
			}
			err = fmt.Errorf("%s is directory", p)
		}
	}
	return "", err
}

func installService(name, desc string) error {
	exepath, err := exePath()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", name)
	}
	s, err = m.CreateService(name, exepath, mgr.Config{DisplayName: desc})
	if err != nil {
		return err
	}
	defer s.Close()
	err = eventlog.InstallAsEventCreate(name, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil {
		s.Delete()
		return fmt.Errorf("SetupEventLogSource() failed: %s", err)
	}
	return nil
}

func removeService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("service %s is not installed", name)
	}
	defer s.Close()
	err = s.Delete()
	if err != nil {
		return err
	}
	err = eventlog.Remove(name)
	if err != nil {
		return fmt.Errorf("RemoveEventLogSource() failed: %s", err)
	}
	return nil
}

func startService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("could not access service: %v", err)
	}
	defer s.Close()
	err = s.Start("is", "manual-started")
	if err != nil {
		return fmt.Errorf("could not start service: %v", err)
	}
	return nil
}

func controlService(name string, c svc.Cmd, to svc.State) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("could not access service: %v", err)
	}
	defer s.Close()
	status, err := s.Control(c)
	if err != nil {
		return fmt.Errorf("could not send control=%d: %v", c, err)
	}
	timeout := time.Now().Add(10 * time.Second)
	for status.State != to {
		if timeout.Before(time.Now()) {
			return fmt.Errorf("timeout waiting for service to go to state=%d", to)
		}
		time.Sleep(300 * time.Millisecond)
		status, err = s.Query()
		if err != nil {
			return fmt.Errorf("could not retrieve service status: %v", err)
		}
	}
	return nil
}


func runService(name string, isDebug bool) {
	var err error
	var elog debug.Log
	if isDebug {
		elog = debug.New(name)
	} else {
		elog, err = eventlog.Open(name)
		if err != nil {
			return
		}
	}
	defer elog.Close()

	elog.Info(1, fmt.Sprintf("starting %s service", name))
	run := svc.Run
	if isDebug {
		run = debug.Run
	}
	err = run(name, &apiserv{&elog,name})
	if err != nil {
		elog.Error(1, fmt.Sprintf("%s service failed: %v", name, err))
		return
	}
	elog.Info(1, fmt.Sprintf("%s service stopped", name))
}

type apiservestatus struct {
	l sync.RWMutex
	run bool
}
func (as *apiservestatus) GetRun() (bool) {
	as.l.RLock()
	defer as.l.RUnlock()
	return as.run
}
func (as *apiservestatus) SetRun(run bool)  {
	as.l.Lock()
	defer as.l.Unlock()
	as.run = false
}

type apiservehttp struct {
	as * apiservestatus
}

func (ah *apiservehttp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status := *(ah.as)
	w.Header().Set("Content-Type", "text/xml")
	if status.GetRun() {
		fmt.Fprintf(w, "<?xml version='1.0' encoding='UTF-8'?><response><state>Doing something</state></response>")
	} else {
		fmt.Fprintf(w, "<?xml version='1.0' encoding='UTF-8'?><response><state>Stop</state></response>")
	}
}

type apiserv struct{
	elog *debug.Log
	name string
}

func (s *apiserv) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	elog := (*s.elog)
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPauseAndContinue
	changes <- svc.Status{State: svc.StartPending}
	
	as := apiservestatus{run:true}
	server := manners.NewWithServer(&http.Server{Addr: ":14487", Handler: &apiservehttp{&as}})
	
	go func(server *manners.GracefulServer) {
		server.ListenAndServe()
	}(server)
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}
	
	tick := time.Tick(5 * time.Second)
	service:
	for {
		select {
			case c := <-r:
				switch c.Cmd {
					case svc.Interrogate:
						changes <- c.CurrentStatus
					case svc.Stop, svc.Shutdown:
						as.SetRun(false)
						elog.Error(1,"Stopping")
						break service
					case svc.Pause:
					case svc.Continue:
					default:
						elog.Error(1, fmt.Sprintf("unexpected control request #%d", c))
				}
			case <-tick:
				elog.Info(1, "Alive")
		}
	}
	changes <- svc.Status{State: svc.StopPending}
	elog.Info(1,"Sleeping 5 seconds for finishing off calls")
	time.Sleep(5 * time.Second)
	elog.Info(1,"Sleeping with manners")
	server.BlockingClose()
	elog.Info(1,"Done")
	return
}

func usage(errmsg string) {
	log.Printf("Error %s\nUse %s install, remove, debug, start, stop, pause or continue.\n",errmsg, os.Args[0])
	os.Exit(2)
}


func main() {
	const svcName = "apiserv"
	
	var err error
	
	if len(os.Args) < 2 {
		isIntSess, err := svc.IsAnInteractiveSession()
		if err != nil {
			//log.Printf("failed to determine if we are running in an interactive session: %v", err)
			isIntSess = false
		}

		if isIntSess {
			log.Printf("Is interactive, running debug")
			runService(svcName, true)
		} else {
			runService(svcName, false)
		}
	} else {
		cmd := strings.ToLower(os.Args[1])
		switch cmd {
			case "install":
				err = installService(svcName, svcName)
			case "remove":
				err = removeService(svcName)
			case "start":
				err = startService(svcName)
			case "stop":
				err = controlService(svcName, svc.Stop, svc.Stopped)
			case "pause":
				err = controlService(svcName, svc.Pause, svc.Paused)
			case "continue":
				err = controlService(svcName, svc.Continue, svc.Running)
			default:
				usage(fmt.Sprintf("invalid command %s", cmd))
		}
		if err != nil {
			log.Printf("Something went wrong %s",err)
			os.Exit(2)
		}
	}
}