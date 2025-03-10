# Copyright (C) 2019-2022 Zilliz. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
# with the License. You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software distributed under the License
# is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
# or implied. See the License for the specific language governing permissions and limitations under the License.

FROM ubuntu:focal-20220426

ARG TARGETARCH

RUN if [ "$TARGETARCH" = "arm64" ]; then apt-get update && apt-get install -y ninja-build; fi

RUN apt-get update && apt-get install -y --no-install-recommends wget curl ca-certificates gnupg2 \
    g++ gcc gdb gdbserver git make ccache libssl-dev zlib1g-dev zip unzip \
    clang-format-10 clang-tidy-10 lcov libtool m4 autoconf automake python3 python3-pip \
    pkg-config uuid-dev libaio-dev  && \
    apt-get remove --purge -y && \
    rm -rf /var/lib/apt/lists/*

RUN pip3 install conan==1.58.0

RUN echo "target arch $TARGETARCH"
RUN wget -qO- "https://cmake.org/files/v3.24/cmake-3.24.4-linux-`uname -m`.tar.gz" | tar --strip-components=1 -xz -C /usr/local

# Install Go
ENV GOPATH /go
ENV GOROOT /usr/local/go
ENV GO111MODULE on
ENV PATH $GOPATH/bin:$GOROOT/bin:$PATH
RUN mkdir -p /usr/local/go && wget -qO- "https://go.dev/dl/go1.20.7.linux-$TARGETARCH.tar.gz" | tar --strip-components=1 -xz -C /usr/local/go && \
    mkdir -p "$GOPATH/src" "$GOPATH/bin" && \
    go clean --modcache && \
    chmod -R 777 "$GOPATH" && chmod -R a+w $(go env GOTOOLDIR)

# refer: https://code.visualstudio.com/docs/remote/containers-advanced#_avoiding-extension-reinstalls-on-container-rebuild
RUN mkdir -p /home/milvus/.vscode-server/extensions \
        /home/milvus/.vscode-server-insiders/extensions \
    && chmod -R 777 /home/milvus

COPY --chown=0:0 build/docker/builder/entrypoint.sh /

ENTRYPOINT [ "/entrypoint.sh" ]
CMD ["tail", "-f", "/dev/null"]
