FROM debian:12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    autoconf automake bison build-essential ca-certificates cmake curl file \
    flex git libtool nasm ninja-build patchelf pkg-config python3 python3-jinja2 \
    tar unzip zip \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace
