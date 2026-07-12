"use strict";function check(a,b){if(a!==b)throw a+" !== "+b;} function f(){return this;}check(f.call(5),5);check(f.call('x'),'x');console.log('PASS');
