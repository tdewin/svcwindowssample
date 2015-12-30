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
	err = run(name, &APIServ{&elog,name})
	if err != nil {
		elog.Error(1, fmt.Sprintf("%s service failed: %v", name, err))
		return
	}
	elog.Info(1, fmt.Sprintf("%s service stopped", name))
}

type APIServeStatus struct {
	l sync.RWMutex
	run bool
	stopped bool
	err error
}
func (as *APIServeStatus) GetRun() (bool) {
	as.l.RLock()
	defer as.l.RUnlock()
	return as.run
}
func (as *APIServeStatus) SetRun(run bool)  {
	as.l.Lock()
	defer as.l.Unlock()
	as.run = run
}

func (as *APIServeStatus) GetStopped() (bool) {
	as.l.RLock()
	defer as.l.RUnlock()
	return as.stopped
}
func (as *APIServeStatus) GetError() (error) {
	as.l.RLock()
	defer as.l.RUnlock()
	return as.err
}
func (as *APIServeStatus) SetStopped(stopped bool,err error)  {
	as.l.Lock()
	defer as.l.Unlock()
	as.stopped = stopped
	if err != nil {
		as.err = err
	}	
}

type APIServeHTTP struct {
	as * APIServeStatus
}

func (ah *APIServeHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status := *(ah.as)
	w.Header().Set("Content-Type", "text/xml")
	if status.GetRun() {
		fmt.Fprintf(w, "<?xml version='1.0' encoding='UTF-8'?><response><state>Doing something</state></response>")
	} else {
		fmt.Fprintf(w, "<?xml version='1.0' encoding='UTF-8'?><response><state>Stop</state></response>")
	}
}
type APIServ struct{
	elog *debug.Log
	name string
}

func (s *APIServ) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	elog := (*s.elog)
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPauseAndContinue
	changes <- svc.Status{State: svc.StartPending}
	
	port := 14487
	
	as := APIServeStatus{run:true,stopped:false}
	server := manners.NewWithServer(&http.Server{Addr: fmt.Sprintf(":%d",port), Handler: &APIServeHTTP{&as}})
	
	go func(server *manners.GracefulServer,as *APIServeStatus) {
		err := server.ListenAndServe()
		if err != nil {
			as.SetStopped(true,err)
		}
	}(server,&as)
	
	//self testing
	selfurl := fmt.Sprintf("http://localhost:%d",port)
	time.Sleep(100 * time.Millisecond)
	elog.Info(1,fmt.Sprintf("Trying first selftest"))
	_, selferr := http.Get(selfurl)
	for test := 5;test > 0 && selferr != nil;test--  {
		time.Sleep(100 * time.Millisecond)
		elog.Info(1,fmt.Sprintf("Trying selftest %d",test))
		_, selferr = http.Get(selfurl)
	}
	
	if selferr == nil &&  as.GetStopped() == false {
		changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}
		elog.Info(1,"Service gave no error so far and self test was able to get main page")
		tick := time.Tick(1 * time.Second)
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
					if as.GetStopped() {
						elog.Error(1,"Service failed")
						as.SetRun(false)
						break service
					}
			}
		}
	} else {
		as.SetStopped(true,selferr)
	}
	changes <- svc.Status{State: svc.StopPending}
	if !as.GetStopped() {
		elog.Info(1,"Sleeping 5 seconds for finishing off calls")
		time.Sleep(5 * time.Second)
		elog.Info(1,"Sleeping with manners")
		server.BlockingClose()
		elog.Info(1,"Done")
	}  else if lerr := as.GetError(); lerr != nil {
		elog.Error(1,fmt.Sprintf("Error %s",lerr))
	}
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