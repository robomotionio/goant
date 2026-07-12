"use strict";function check(a,b){if(a!==b)throw a+" !== "+b;} var x=(function foo(){return typeof foo;})();check(x,'function');console.log('PASS');
