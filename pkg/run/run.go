// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package run

import (
	"fmt"
	"os"
	"path"

	"github.com/oklog/run"
	"github.com/spf13/pflag"
	"go.uber.org/multierr"
	"go.uber.org/zap"

	"github.com/apache/skywalking-banyandb/pkg/config"
	"github.com/apache/skywalking-banyandb/pkg/logger"
)

// FlagSet holds a pflag.FlagSet as well as an exported Name variable for
// allowing improved help usage information.
type FlagSet struct {
	*pflag.FlagSet
	Name string
}

// NewFlagSet returns a new FlagSet for usage in Config objects.
func NewFlagSet(name string) *FlagSet {
	return &FlagSet{
		FlagSet: pflag.NewFlagSet(name, pflag.ContinueOnError),
		Name:    name,
	}
}

// Unit is the default interface an object needs to implement for it to be able
// to register with a Group.
// Name should return a short but good identifier of the Unit.
type Unit interface {
	Name() string
}

// Config interface should be implemented by Group Unit objects that manage
// their own configuration through the use of flags.
// If a Unit's Validate returns an error it will stop the Group immediately.
type Config interface {
	//Unit for Group registration and identification
	Unit
	//FlagSet returns an object's FlagSet
	FlagSet() *FlagSet
	//Validate checks an object's stored values
	Validate() error
}

// PreRunner interface should be implemented by Group Unit objects that need
// a pre run stage before starting the Group Services.
// If a Unit's PreRun returns an error it will stop the Group immediately.
type PreRunner interface {
	//Unit for Group registration and identification
	Unit
	PreRun() error
}

// NewPreRunner takes a name and a standalone pre runner compatible function
// and turns them into a Group compatible PreRunner, ready for registration.
func NewPreRunner(name string, fn func() error) PreRunner {
	return preRunner{name: name, fn: fn}
}

type preRunner struct {
	name string
	fn   func() error
}

func (p preRunner) Name() string {
	return p.name
}

func (p preRunner) PreRun() error {
	return p.fn()
}

// Service interface should be implemented by Group Unit objects that need
// to run a blocking service until an error occurs or a shutdown request is
// made.
// The Serve method must be blocking and return an error on unexpected shutdown.
// Recoverable errors need to be handled inside the service itself.
// GracefulStop must gracefully stop the service and make the Serve call return.
//
// Since Service is managed by Group, it is considered a design flaw to call any
// of the Service methods directly in application code.
type Service interface {
	// Unit for Group registration and identification
	Unit
	// Serve starts the GroupService and blocks.
	Serve() error
	// GracefulStop shuts down and cleans up the GroupService.
	GracefulStop()
}

// Group builds on https://github.com/oklog/run to provide a deterministic way
// to manage service lifecycles. It allows for easy composition of elegant
// monoliths as well as adding signal handlers, metrics services, etc.
type Group struct {
	// Name of the Group managed service. If omitted, the binaryname will be
	// used as found at runtime.
	Name string

	f   *FlagSet
	r   run.Group
	c   []Config
	p   []PreRunner
	s   []Service
	log *logger.Logger

	showRunGroup bool

	configured bool
}

// Register will inspect the provided objects implementing the Unit interface to
// see if it needs to register the objects for any of the Group bootstrap
// phases. If a Unit doesn't satisfy any of the bootstrap phases it is ignored
// by Group.
// The returned array of booleans is of the same size as the amount of provided
// Units, signalling for each provided Unit if it successfully registered with
// Group for at least one of the bootstrap phases or if it was ignored.
func (g *Group) Register(units ...Unit) []bool {
	g.log = logger.GetLogger(g.Name)
	hasRegistered := make([]bool, len(units))
	for idx := range units {
		if !g.configured {
			// if RunConfig has been called we can no longer register Config
			// phases of Units
			if c, ok := units[idx].(Config); ok {
				g.c = append(g.c, c)
				hasRegistered[idx] = true
			}
		}
		if p, ok := units[idx].(PreRunner); ok {
			g.p = append(g.p, p)
			hasRegistered[idx] = true
		}
		if s, ok := units[idx].(Service); ok {
			g.s = append(g.s, s)
			hasRegistered[idx] = true
		}
	}
	return hasRegistered
}

func (g *Group) RegisterFlags() *FlagSet {
	// run configuration stage
	g.f = NewFlagSet(g.Name)
	g.f.SortFlags = false // keep order of flag registration
	g.f.Usage = func() {
		fmt.Printf("Flags:\n")
		g.f.PrintDefaults()
	}

	gFS := NewFlagSet("Common Service options")
	gFS.SortFlags = false
	gFS.StringVarP(&g.Name, "name", "n", g.Name, `name of this service`)
	gFS.BoolVar(&g.showRunGroup, "show-rungroup-units", false, "show rungroup units")
	g.f.AddFlagSet(gFS.FlagSet)

	// register flags from attached Config objects
	fs := make([]*FlagSet, len(g.c))
	for idx := range g.c {
		// a Namer might have been deregistered
		if g.c[idx] == nil {
			continue
		}
		nameField := logger.String("name", g.c[idx].Name())
		indexField := logger.Uint32("registered", uint32(idx+1))
		g.log.Debug("register flags", nameField, indexField,
			logger.Uint32("total", uint32(len(g.c))))
		fs[idx] = g.c[idx].FlagSet()
		if fs[idx] == nil {
			// no FlagSet returned
			g.log.Debug("config object did not return a flagset", nameField)
			continue
		}
		fs[idx].VisitAll(func(f *pflag.Flag) {
			if g.f.Lookup(f.Name) != nil {
				// log duplicate flag
				g.log.Warn("ignoring duplicate flag", logger.String("name", f.Name), indexField)
				return
			}
			g.f.AddFlag(f)
		})
	}
	return g.f
}

// RunConfig runs the Config phase of all registered Config aware Units.
// Only use this function if needing to add additional wiring between config
// and (pre)run phases and a separate PreRunner phase is not an option.
// In most cases it is best to use the Run method directly as it will run the
// Config phase prior to executing the PreRunner and Service phases.
// If an error is returned the application must shut down as it is considered
// fatal.
func (g *Group) RunConfig() (interrupted bool, err error) {
	g.log = logger.GetLogger(g.Name)
	g.configured = true

	if g.Name == "" {
		// use the binary name if custom name has not been provided
		g.Name = path.Base(os.Args[0])
	}

	defer func() {
		if err != nil {
			g.log.Error("unexpected exit", logger.Error(err))
		}
	}()

	// Load config from env and file
	if err = config.Load(g.f.Name, g.f.FlagSet); err != nil {
		return false, err
	}

	// bail early on help or version requests
	switch {
	case g.showRunGroup:
		fmt.Println(g.ListUnits())
		return true, nil
	}

	// Validate Config inputs
	for idx := range g.c {
		// a Config might have been deregistered during Run
		indexField := logger.Uint32("ran", uint32(idx+1))
		if g.c[idx] == nil {
			g.log.Debug("skipping validate", indexField)
			continue
		}
		g.log.Debug("validate config", logger.String("name", g.c[idx].Name()), indexField,
			logger.Uint32("total", uint32(len(g.c))))
		if vErr := g.c[idx].Validate(); vErr != nil {
			err = multierr.Append(err, vErr)
		}
	}

	// exit on at least one Validate error
	if err != nil {
		return false, err
	}

	// log binary name and version
	g.log.Info("started")

	return false, nil
}

// Run will execute all phases of all registered Units and block until an error
// occurs.
// If RunConfig has been called prior to Run, the Group's Config phase will be
// skipped and Run continues with the PreRunner and Service phases.
//
// The following phases are executed in the following sequence:
//
//   Config phase (serially, in order of Unit registration)
//     - FlagSet()        Get & register all FlagSets from Config Units.
//     - Flag Parsing     Using the provided args (os.Args if empty)
//     - Validate()       Validate Config Units. Exit on first error.
//
//   PreRunner phase (serially, in order of Unit registration)
//     - PreRun()         Execute PreRunner Units. Exit on first error.
//
//   Service phase (concurrently)
//     - Serve()          Execute all Service Units in separate Go routines.
//     - Wait             Block until one of the Serve() methods returns
//     - GracefulStop()   Call interrupt handlers of all Service Units.
//
//   Run will return with the originating error on:
//   - first Config.Validate()  returning an error
//   - first PreRunner.PreRun() returning an error
//   - first Service.Serve()    returning (error or nil)
//
func (g *Group) Run() (err error) {
	// run config registration and flag parsing stages
	if interrupted, err := g.RunConfig(); interrupted || err != nil {
		return err
	}
	defer func() {
		if err != nil {
			g.log.WithOptions(zap.AddStacktrace(zap.FatalLevel)).Error("unexpected exit", logger.Error(err))
		}
	}()

	// execute pre run stage and exit on error
	for idx := range g.p {
		// a PreRunner might have been deregistered during Run
		if g.p[idx] == nil {
			continue
		}
		g.log.Debug("pre-run:", logger.String("name", g.p[idx].Name()),
			logger.Uint32("ran", uint32(idx+1)), logger.Uint32("total", uint32(len(g.p))))
		if err := g.p[idx].PreRun(); err != nil {
			return err
		}
	}

	// feed our registered services to our internal run.Group
	for idx := range g.s {
		// a Service might have been deregistered during Run
		s := g.s[idx]
		if s == nil {
			continue
		}

		g.log.Debug("serve:", logger.String("name", s.Name()),
			logger.Uint32("ran", uint32(idx+1)), logger.Uint32("total", uint32(len(g.s))))
		g.r.Add(func() error {
			return s.Serve()
		}, func(_ error) {
			g.log.Debug("stop:", logger.String("name", s.Name()),
				logger.Uint32("ran", uint32(idx+1)), logger.Uint32("total", uint32(len(g.s))))
			s.GracefulStop()
		})
	}

	// start registered services and block
	return g.r.Run()
}

// ListUnits returns a list of all Group phases and the Units registered to each
// of them.
func (g Group) ListUnits() string {
	var (
		s string
		t = "cli"
	)

	if len(g.c) > 0 {
		s += "\n- config: "
		for _, u := range g.c {
			if u != nil {
				s += u.Name() + " "
			}
		}
	}
	if len(g.p) > 0 {
		s += "\n- prerun: "
		for _, u := range g.p {
			if u != nil {
				s += u.Name() + " "
			}
		}
	}
	if len(g.s) > 0 {
		s += "\n- serve : "
		for _, u := range g.s {
			if u != nil {
				t = "svc"
				s += u.Name() + " "
			}
		}
	}

	return fmt.Sprintf("Group: %s [%s]%s", g.Name, t, s)
}
