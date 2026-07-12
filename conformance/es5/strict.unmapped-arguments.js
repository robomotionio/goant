"use strict";function check(a,b){if(a!==b)throw a+" !== "+b;} function f(){return arguments.length;}check(f(1,2),2);console.log('PASS');
