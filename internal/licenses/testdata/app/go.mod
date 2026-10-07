module example.com/app

go 1.21

require (
	example.com/bsdlib v0.0.0
	example.com/mitlib v0.0.0
	example.com/testonly v0.0.0
	example.com/unknown v0.0.0
	example.com/unlicensed v0.0.0
)

replace (
	example.com/bsdlib => ../bsdlib
	example.com/mitlib => ../mitlib
	example.com/testonly => ../testonly
	example.com/unknown => ../unknown
	example.com/unlicensed => ../unlicensed
)
