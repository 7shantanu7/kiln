# kiln.cmake: add kiln to a CMake project without editing its build files.
#
#   cmake -S . -B build -G Ninja -DCMAKE_PROJECT_INCLUDE=/path/to/kiln.cmake
#
# Options (cache variables):
#   KILN_PREFIX_MAP      ON   Rewrite absolute source and build paths with
#                             -ffile-prefix-map. Makes objects reproducible across
#                             checkouts and lets kiln-cc share debug builds.
#   KILN_CACHE_ADDR      ""   Address of kiln-cache or any Remote Execution API
#                             cache. When set, kiln-cc becomes the compiler launcher.
#                             Defaults to the KILN_CACHE_ADDR environment variable.
#   KILN_CACHE_READONLY  OFF  Look up but never upload (use for pull requests).
#   KILN_CC                   Path to kiln-cc (default: searched on PATH).
#
# For reproducible timestamps, export SOURCE_DATE_EPOCH for both configure and
# build: CMake's string(TIMESTAMP) and the compilers' __DATE__/__TIME__ honor it.

include_guard(GLOBAL)

option(KILN_PREFIX_MAP "Map absolute source/build paths with -ffile-prefix-map" ON)
set(KILN_CACHE_ADDR "$ENV{KILN_CACHE_ADDR}" CACHE STRING "kiln-cache address (empty disables caching)")
option(KILN_CACHE_READONLY "Look up in the cache but never upload" OFF)

if(KILN_PREFIX_MAP)
  # The compiler applies the last matching map, so the build directory (which
  # may sit inside the source tree) goes last.
  add_compile_options(
    "-ffile-prefix-map=${CMAKE_SOURCE_DIR}=."
    "-ffile-prefix-map=${CMAKE_BINARY_DIR}=build")
endif()

if(KILN_CACHE_ADDR)
  find_program(KILN_CC kiln-cc REQUIRED)
  set(_kiln_launcher "${KILN_CC}"
    "--addr=${KILN_CACHE_ADDR}"
    "--base-dir=${CMAKE_SOURCE_DIR}"
    "--base-dir=${CMAKE_BINARY_DIR}")
  if(KILN_CACHE_READONLY)
    list(APPEND _kiln_launcher "--read-only")
  endif()
  foreach(_kiln_lang C CXX)
    if(NOT CMAKE_${_kiln_lang}_COMPILER_LAUNCHER)
      set(CMAKE_${_kiln_lang}_COMPILER_LAUNCHER ${_kiln_launcher})
    endif()
  endforeach()
  message(STATUS "kiln: caching compiles in ${KILN_CACHE_ADDR}")
endif()
