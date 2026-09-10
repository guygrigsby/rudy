package turn

import "runtime"

// runtimeOS is the platform a prompt's ${os} names. It is a variable so a test can pin it.
var runtimeOS = runtime.GOOS
