## dlv exec

Runs precompiled binary, attaches and begins debug session.

### Synopsis


Runs precompiled binary, attaches and begins debug session.

```
dlv exec [./path/to/binary]
```

### Options inherited from parent commands

```
      --accept-multiclient[=false]: Allows a headless server to accept multiple client connection. Note that the server API is not reentrant and clients will have to coordinate
      --build-flags="": Build flags, to be passed to the compiler.
      --headless[=false]: Run debug server only, in headless mode.
      --init="": Init file, executed by the terminal client.
  -l, --listen="localhost:0": Debugging server listen address.
      --log[=false]: Enable debugging server logging.
```

### SEE ALSO
* [dlv](dlv.md)	 - Delve is a debugger for the Go programming language.

###### Auto generated by spf13/cobra on 19-Feb-2016