# Shared codegen variables for back/Makefile and every services/*/Makefile.
#
# Include it after defining BACK_DIR (the path to back/ from the including
# Makefile):
#
#   BACK_DIR := ../..
#   include $(BACK_DIR)/tools/codegen.mk
#
# BUF_VERSION is the one pin (tools/buf.version); back/ uses it for the
# bufbuild/buf Docker image and tools/codegen.sh for the release binary.
# Exporting BUF_VERSION and TOOLS_DIR (the binary cache) makes the pin and both
# overrides reach codegen.sh. BUF=<path to an existing buf binary> reaches it on
# its own, from the environment or the make command line; back/Makefile calls
# its Docker command BUF_DOCKER so the two never collide.

TOOLS_SRC   := $(BACK_DIR)/tools
BUF_VERSION ?= $(shell cat $(TOOLS_SRC)/buf.version)
CODEGEN     := $(TOOLS_SRC)/codegen.sh
GEN_GO_DIR  := $(BACK_DIR)/gen/go

export BUF_VERSION
export TOOLS_DIR
