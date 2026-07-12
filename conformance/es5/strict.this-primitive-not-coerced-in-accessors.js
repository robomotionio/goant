"use strict";function check(a,b){if(a!==b)throw a+" !== "+b;} var o={get x(){return this;}};check(o.x.get===undefined,true);console.log('PASS');
