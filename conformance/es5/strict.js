"use strict";function check(a,b){if(a!==b)throw a+" !== "+b;} var x=1;check(x,1);check((function(){return this;})(),undefined);console.log('PASS');
