# Copyright 2025 Robert Bosch GmbH
#
# SPDX-License-Identifier: Apache-2.0

export TASK_X_REMOTE_TASKFILES=1
export DSE_REPORT_IMAGE=ghcr.io/boschglobal/dse-report:latest
export DSE_BUILDER_IMAGE=ghcr.io/boschglobal/dse-builder:latest


default: build
.PHONY: build
build:
	dse-parse2ast runnable.dse runnable.json
	dse-ast convert -input runnable.json -output runnable.yaml
	dse-ast resolve -input runnable.yaml
	dse-ast generate -input runnable.yaml -output .

# FIXME Metadata retrieval not working. Networking issue. Need more debug logging.
.PHONY: build-docker
build-docker:
	docker run -it --rm \
		-v $$(pwd):/workdir \
		$(DSE_BUILDER_IMAGE) runnable.dse

.PHONY: task
task:
	task -y -v
	ls -R out/sim
	ls -R out/fmu

.PHONY: report
report:
	docker run -it --rm \
		-v $$(pwd)/out/sim:/sim \
		$(DSE_REPORT_IMAGE) /sim

.PHONY: simer
simer:
	docker run -it --rm \
		-v $$(pwd)/out/sim:/sim \
		$(DSE_SIMER_IMAGE) \
			-redis="" \
			-simbus="" \
			-transport="loopback" \
			-uri="loopback" \
        	-env SIMBUS_LOGLEVEL=2 \
        	-logger 1

.PHONY: simer-modelc-runtime
simer-modelc-runtime:
	docker run -it --rm \
		-v $$(pwd)/out/sim:/sim \
		--entrypoint="" \
		$(DSE_SIMER_IMAGE) \
			bin/importer \
			lib/libruntime.so \
			sim data/simulation.yaml runnable

.PHONY: clean
clean:
	rm -rf out/sim
	rm -rf out/fmu

.PHONY: cleanall
cleanall:
	git clean -Xdf
