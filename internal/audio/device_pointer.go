package audio

/*
#include <stdlib.h>
*/
import "C"

import "unsafe"

func freeDevicePointer(pointer unsafe.Pointer) {
	C.free(pointer)
}
