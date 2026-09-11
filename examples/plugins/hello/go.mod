// The example is its own module so that `rudy install examples/plugins/hello` works: the
// install copies this directory alone into a stage under the data root, outside every other
// module, and the manifest's `go build -o hello .` needs a go.mod there to build at all.
module github.com/guygrigsby/rudy/examples/plugins/hello

go 1.26.2
