"use strict";function check(a,b){if(a!==b)throw a+" !== "+b;} function f(){return this;}check(f(),undefined);console.log('PASS');
