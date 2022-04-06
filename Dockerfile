# Copyright 2019 PingCAP, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# See the License for the specific language governing permissions and
# limitations under the License.

FROM debian:stable-slim

RUN apt-get install tzdata
RUN apt-get update && apt-get upgrade -y && apt-get install -y ca-certificates


ADD output/bin/darwin/amd64/cmd/pd-discovery /usr/local/bin/pd-discovery
ADD output/bin/darwin/amd64/cmd/tikv-controller-manager /usr/local/bin/tikv-controller-manager
