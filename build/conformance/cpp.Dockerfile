# The C++ SDK's conformance client of Agones (test/sdk/cpp), unchanged,
# built the way build/build-sdk-images/cpp/build-sdk-test.sh of Agones
# builds it. The SDK accepts one version of gRPC only, the one Agones
# builds from source (grpc_release_tag in build/includes/sdk.mk). That
# build takes the better part of an hour, and is its own stage so that a
# cache keeps it.
FROM debian:trixie AS grpc
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential cmake git ca-certificates pkg-config && \
    rm -rf /var/lib/apt/lists/*
RUN git clone --recurse-submodules -b v1.83.0 --depth 1 --shallow-submodules \
      https://github.com/grpc/grpc /grpc && \
    cmake -S /grpc -B /grpc/cmake/build -DCMAKE_BUILD_TYPE=Release \
      -DgRPC_INSTALL=ON -DgRPC_BUILD_TESTS=OFF -DCMAKE_INSTALL_PREFIX=/usr/local && \
    cmake --build /grpc/cmake/build --target install -j"$(nproc)" && \
    rm -rf /grpc

FROM grpc AS build
ARG agones=third_party/agones
WORKDIR /agones
COPY ${agones}/sdks/cpp sdks/cpp
COPY ${agones}/test/sdk/cpp test/sdk/cpp
RUN cmake -S sdks/cpp -B /sdk -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/sdk/install -Wno-dev && \
    cmake --build /sdk --target install -j"$(nproc)" && \
    cmake -S test/sdk/cpp -B /client -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_PREFIX_PATH=/sdk/install -Dagones_DIR=/sdk/install/agones/cmake \
      -DCMAKE_INSTALL_PREFIX=/client/install && \
    cmake --build /client --target install -j"$(nproc)"

FROM debian:trixie-slim
COPY --from=build /client/install/cpp-simple /client
ENTRYPOINT ["/client"]
