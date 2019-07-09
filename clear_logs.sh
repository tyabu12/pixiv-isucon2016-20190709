#!/bin/bash

set -x

sudo truncate --size 0 /var/log/nginx/access.log
sudo truncate --size 0 /var/log/nginx/error.log

sudo truncate --size 0 /var/log/mysql/mysql-slow.log
sudo truncate --size 0 /var/log/mysql/error.log

