#!/bin/bash

set -eux

git pull --ff-only

./webapp/golang/setup.sh

sudo nginx -t
sudo nginx -s reload

sudo systemctl restart mysql
sudo systemctl restart isu-go

./clear_logs.sh

