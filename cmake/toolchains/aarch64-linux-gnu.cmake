# Cross-compile for aarch64 Linux with the Debian/Ubuntu cross toolchain
# (apt install g++-aarch64-linux-gnu qemu-user-static). Tests run under QEMU
# user-mode emulation, which CTest uses for tests added by target name.
#
#   cmake -DCMAKE_TOOLCHAIN_FILE=cmake/toolchains/aarch64-linux-gnu.cmake ...
set(CMAKE_SYSTEM_NAME Linux)
set(CMAKE_SYSTEM_PROCESSOR aarch64)
set(CMAKE_C_COMPILER aarch64-linux-gnu-gcc)
set(CMAKE_CXX_COMPILER aarch64-linux-gnu-g++)

set(CMAKE_FIND_ROOT_PATH /usr/aarch64-linux-gnu)
set(CMAKE_FIND_ROOT_PATH_MODE_PROGRAM NEVER) # build tools (Python, codegen) run on the host
set(CMAKE_FIND_ROOT_PATH_MODE_LIBRARY ONLY)
set(CMAKE_FIND_ROOT_PATH_MODE_INCLUDE ONLY)
set(CMAKE_FIND_ROOT_PATH_MODE_PACKAGE ONLY)

# No emulator needed when the host is already aarch64.
if(NOT CMAKE_HOST_SYSTEM_PROCESSOR MATCHES "^(aarch64|arm64)$")
  find_program(KILN_QEMU_AARCH64 NAMES qemu-aarch64-static qemu-aarch64 REQUIRED)
  set(CMAKE_CROSSCOMPILING_EMULATOR "${KILN_QEMU_AARCH64};-L;/usr/aarch64-linux-gnu")
endif()
