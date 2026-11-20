#!/bin/sh

is_even() {
  [ "$(( $1 % 2 ))" -eq 1 ]
}

is_even 2
