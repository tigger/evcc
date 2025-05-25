#!/bin/sh

make docker DOCKER_IMAGE=tigger/evcc DOCKER_TAG=eprowallbox PLATFORM=linux/amd64 BUILD_MODE=--load
