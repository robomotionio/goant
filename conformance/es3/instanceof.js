function check(a,b){if(a!==b)throw a+" !== "+b;} check([] instanceof Array,true);check([] instanceof Object,true);check({} instanceof Array,false);console.log('PASS');
